package extensions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const (
	installCacheVersion = "objects-v1"
	installLockWait     = 30 * time.Second
)

var installMutexes [64]sync.Mutex

// DefaultInstallRoot returns Switchboard's user plugin cache. It does not
// create the directory or inspect any plugins.
func DefaultInstallRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating user home: %w", err)
	}
	if strings.TrimSpace(home) == "" || hasControl(home) {
		return "", errors.New("user home is empty or contains a control character")
	}
	return filepath.Join(home, ".switchboard", "plugin-cache"), nil
}

// Install copies an already-discovered local plugin into cacheRoot and returns
// a freshly discovered record rooted in the Switchboard-owned cache. The
// destination is content-addressed by plugin ID and digest. Install never runs
// lifecycle scripts, enables components, changes activation state, contacts a
// marketplace, or performs network access.
func Install(plugin Plugin, cacheRoot string) (Plugin, error) {
	return InstallContext(context.Background(), plugin, cacheRoot)
}

// InstallContext is Install with cancellation across lock waits and bounded
// file copies. Publishing remains atomic: cancellation never exposes a
// partially copied plugin.
func InstallContext(ctx context.Context, plugin Plugin, cacheRoot string) (Plugin, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Plugin{}, err
	}
	if err := validateInstallInput(plugin); err != nil {
		return Plugin{}, err
	}
	cache, cachePath, err := openInstallCache(cacheRoot)
	if err != nil {
		return Plugin{}, err
	}
	defer cache.Close()
	cacheDirectory, err := openPinnedInstallCacheDirectory(cache)
	if err != nil {
		return Plugin{}, err
	}
	defer cacheDirectory.Close()

	objectRel := installDestination(plugin)
	lock := installMutex(plugin, cachePath, objectRel)
	lock.Lock()
	defer lock.Unlock()

	if installed, exists, err := inspectInstalled(cache, cachePath, objectRel, plugin); exists || err != nil {
		return installed, err
	}
	parentRel := path.Dir(objectRel)
	if err := ensureInstallNamespace(cache, parentRel); err != nil {
		return Plugin{}, err
	}

	lockRel := objectRel + ".lock"
	if err := acquireInstallLock(ctx, cache, lockRel); err != nil {
		return Plugin{}, err
	}
	defer func() { _ = cache.Remove(filepath.FromSlash(lockRel)) }()

	// A separate process may have completed while this process waited for the
	// on-disk lock. Never replace what it published.
	if installed, exists, err := inspectInstalled(cache, cachePath, objectRel, plugin); exists || err != nil {
		return installed, err
	}

	source, err := openInstallSource(plugin)
	if err != nil {
		return Plugin{}, err
	}
	defer source.Close()
	if err := inspectExactPlugin(plugin.RealPath, source, plugin, "source"); err != nil {
		return Plugin{}, err
	}

	stageRel, err := makeInstallStage(cache, parentRel)
	if err != nil {
		return Plugin{}, err
	}
	stagePresent := true
	defer func() {
		if stagePresent {
			_ = cache.RemoveAll(filepath.FromSlash(stageRel))
		}
	}()

	stageObject, err := openInstallSubdirectory(cache, filepath.FromSlash(stageRel))
	if err != nil {
		return Plugin{}, fmt.Errorf("opening staged plugin object: %w", err)
	}
	pluginLeaf := installPluginLeaf(plugin)
	if err := createPrivateInstallDirectory(stageObject, pluginLeaf); err != nil {
		stageObject.Close()
		return Plugin{}, fmt.Errorf("creating staged plugin root: %w", err)
	}
	stage, err := openInstallSubdirectory(stageObject, pluginLeaf)
	if err != nil {
		stageObject.Close()
		return Plugin{}, fmt.Errorf("opening staged plugin root: %w", err)
	}
	budget := digestBudget{}
	copyErr := copyInstallDirectoryContext(ctx, source, stage, "", 0, &budget)
	rootCloseErr := stage.Close()
	objectCloseErr := stageObject.Close()
	if copyErr != nil {
		return Plugin{}, fmt.Errorf("copying plugin %s: %w", plugin.ID, copyErr)
	}
	if rootCloseErr != nil {
		return Plugin{}, fmt.Errorf("closing staged plugin %s: %w", plugin.ID, rootCloseErr)
	}
	if objectCloseErr != nil {
		return Plugin{}, fmt.Errorf("closing staged plugin object %s: %w", plugin.ID, objectCloseErr)
	}

	// Re-read the source after the copy so an updater cannot silently change the
	// catalog tree while an installation is in flight.
	if err := inspectExactPlugin(plugin.RealPath, source, plugin, "source after copy"); err != nil {
		return Plugin{}, err
	}
	stagePluginRel := path.Join(stageRel, pluginLeaf)
	stagePath := filepath.Join(cachePath, filepath.FromSlash(stagePluginRel))
	stageCheck, err := openInstallSubdirectory(cache, filepath.FromSlash(stagePluginRel))
	if err != nil {
		return Plugin{}, fmt.Errorf("reopening staged plugin root: %w", err)
	}
	if err := inspectExactPlugin(stagePath, stageCheck, plugin, "staged copy"); err != nil {
		stageCheck.Close()
		return Plugin{}, err
	}
	if err := stageCheck.Close(); err != nil {
		return Plugin{}, fmt.Errorf("closing verified staged plugin: %w", err)
	}

	if info, err := safeInfo(cache, objectRel); err != nil {
		return Plugin{}, fmt.Errorf("checking install destination: %w", err)
	} else if info != nil {
		installed, _, inspectErr := inspectInstalled(cache, cachePath, objectRel, plugin)
		if inspectErr != nil {
			return Plugin{}, inspectErr
		}
		return installed, nil
	}
	if err := ctx.Err(); err != nil {
		return Plugin{}, err
	}
	if err := publishInstall(cacheDirectory, cache, stageRel, objectRel); err != nil {
		if installed, exists, inspectErr := inspectInstalled(cache, cachePath, objectRel, plugin); exists || inspectErr != nil {
			return installed, inspectErr
		}
		return Plugin{}, fmt.Errorf("publishing plugin %s atomically: %w", plugin.ID, err)
	}
	stagePresent = false

	installed, exists, err := inspectInstalled(cache, cachePath, objectRel, plugin)
	if err != nil {
		return Plugin{}, err
	}
	if !exists {
		return Plugin{}, fmt.Errorf("installed plugin %s disappeared after publication", plugin.ID)
	}
	return installed, nil
}

func openPinnedInstallCacheDirectory(cache *os.Root) (*os.File, error) {
	// Open through the already pinned root. Reopening cachePath by name after
	// validation lets another process replace it with a FIFO and block here.
	cacheDirectory, err := cache.Open(".")
	if err != nil {
		return nil, fmt.Errorf("opening pinned plugin cache directory: %w", err)
	}
	if directoryInfo, statErr := cacheDirectory.Stat(); statErr != nil {
		cacheDirectory.Close()
		return nil, fmt.Errorf("reading plugin cache directory: %w", statErr)
	} else if rootInfo, rootErr := cache.Stat("."); rootErr != nil || !os.SameFile(directoryInfo, rootInfo) {
		cacheDirectory.Close()
		if rootErr != nil {
			return nil, fmt.Errorf("reading pinned plugin cache: %w", rootErr)
		}
		return nil, errors.New("plugin cache directory identity changed while opening")
	}
	return cacheDirectory, nil
}

// InstallActivation is the public activation-capability boundary. It installs
// plugin into Switchboard's content-addressed cache, or verifies the exact
// idempotent destination already there, then returns a capability for that
// cached copy. It does not enable the plugin, grant executable trust, or infer
// permission from native-client state.
func InstallActivation(plugin Plugin, cacheRoot string) (*ActivationCandidate, error) {
	return InstallActivationContext(context.Background(), plugin, cacheRoot)
}

// InstallActivationContext is InstallActivation with a cancellable install
// wait. Existing callers retain the non-cancellable CLI behavior through the
// Background wrapper above.
func InstallActivationContext(ctx context.Context, plugin Plugin, cacheRoot string) (*ActivationCandidate, error) {
	installed, err := InstallContext(ctx, plugin, cacheRoot)
	if err != nil {
		return nil, err
	}
	candidate, err := newActivationCandidate(installed)
	if err != nil {
		return nil, fmt.Errorf("validating installed activation candidate: %w", err)
	}
	return candidate, nil
}

func validateInstallInput(plugin Plugin) error {
	if err := validatePluginIdentity(plugin); err != nil {
		return fmt.Errorf("invalid plugin: %w", err)
	}
	if plugin.Kind != KindPlugin {
		return fmt.Errorf("invalid plugin kind %q", plugin.Kind)
	}
	if err := validateNamespace(plugin.Namespace); err != nil {
		return fmt.Errorf("invalid plugin namespace: %w", err)
	}
	if plugin.ID != string(plugin.Dialect)+":"+plugin.Namespace {
		return errors.New("plugin ID does not exactly match its dialect and namespace")
	}
	if plugin.Root == "" || !filepath.IsAbs(plugin.Root) {
		return errors.New("plugin root must be absolute")
	}
	if hasControl(plugin.Root) || hasControl(plugin.RealPath) {
		return errors.New("plugin source path contains a control character")
	}
	digest, err := hex.DecodeString(plugin.Digest)
	if err != nil || len(digest) != sha256.Size || plugin.Digest != strings.ToLower(plugin.Digest) {
		return errors.New("plugin digest must be a canonical SHA-256 value")
	}
	return nil
}

func openInstallCache(cacheRoot string) (*os.Root, string, error) {
	if strings.TrimSpace(cacheRoot) == "" {
		return nil, "", errors.New("plugin cache root is required")
	}
	if hasControl(cacheRoot) {
		return nil, "", errors.New("plugin cache root contains a control character")
	}
	abs, err := filepath.Abs(cacheRoot)
	if err != nil {
		return nil, "", fmt.Errorf("resolving plugin cache root: %w", err)
	}
	abs = filepath.Clean(abs)
	if err := prepareInstallCacheDirectory(abs); err != nil {
		return nil, "", fmt.Errorf("creating plugin cache root: %w", err)
	}
	pathInfo, err := os.Lstat(abs)
	if err != nil {
		return nil, "", fmt.Errorf("reading plugin cache root: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, "", errors.New("plugin cache root must not be a symbolic link")
	}
	realPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", fmt.Errorf("resolving plugin cache root: %w", err)
	}
	realPath, err = filepath.Abs(realPath)
	if err != nil {
		return nil, "", fmt.Errorf("resolving canonical plugin cache root: %w", err)
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return nil, "", fmt.Errorf("reading plugin cache root: %w", err)
	}
	if !info.IsDir() {
		return nil, "", errors.New("plugin cache root is not a directory")
	}
	if err := validateInstallCacheDirectory(realPath, info); err != nil {
		return nil, "", err
	}
	root, err := openInstallDirectory(realPath)
	if err != nil {
		return nil, "", fmt.Errorf("opening plugin cache root: %w", err)
	}
	if err := ensureRootPath(root, realPath); err != nil {
		root.Close()
		return nil, "", fmt.Errorf("pinning plugin cache root: %w", err)
	}
	return root, filepath.Clean(realPath), nil
}

func ensureInstallNamespace(root *os.Root, rel string) error {
	clean, err := safeRelativePath(rel)
	if err != nil {
		return fmt.Errorf("unsafe plugin cache namespace: %w", err)
	}
	parts := strings.Split(clean, "/")
	for index := range parts {
		prefix := strings.Join(parts[:index+1], "/")
		info, err := root.Lstat(filepath.FromSlash(prefix))
		if os.IsNotExist(err) {
			if err := createPrivateInstallDirectory(root, prefix); err != nil && !os.IsExist(err) {
				return fmt.Errorf("creating plugin cache namespace %q: %w", prefix, err)
			}
			info, err = root.Lstat(filepath.FromSlash(prefix))
		}
		if err != nil {
			return fmt.Errorf("reading plugin cache namespace %q: %w", prefix, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("plugin cache namespace %q is not a physical directory", prefix)
		}
		if err := validatePrivateInstallDirectory(root, prefix, info); err != nil {
			return fmt.Errorf("plugin cache namespace %q is not private: %w", prefix, err)
		}
	}
	return nil
}

func createPrivateInstallDirectory(root *os.Root, rel string) error {
	osPath := filepath.FromSlash(rel)
	if err := root.Mkdir(osPath, 0o700); err != nil {
		return err
	}
	if err := securePrivateInstallDirectory(root, rel); err != nil {
		removeErr := root.Remove(osPath)
		if removeErr != nil && !os.IsNotExist(removeErr) {
			return errors.Join(err, fmt.Errorf("removing incompletely secured plugin directory: %w", removeErr))
		}
		return err
	}
	return nil
}

func openInstallSource(plugin Plugin) (*os.Root, error) {
	rootPath, err := filepath.EvalSymlinks(plugin.Root)
	if err != nil {
		return nil, fmt.Errorf("resolving plugin source root: %w", err)
	}
	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resolving canonical plugin source root: %w", err)
	}
	if filepath.Clean(rootPath) != filepath.Clean(plugin.RealPath) {
		return nil, errors.New("plugin source root no longer resolves to its discovered real path")
	}
	realPath, err := filepath.EvalSymlinks(plugin.RealPath)
	if err != nil {
		return nil, fmt.Errorf("resolving plugin real path: %w", err)
	}
	if filepath.Clean(realPath) != filepath.Clean(plugin.RealPath) {
		return nil, errors.New("plugin real path was replaced or retargeted after discovery")
	}
	root, err := openInstallDirectory(plugin.RealPath)
	if err != nil {
		return nil, fmt.Errorf("opening plugin source: %w", err)
	}
	if err := ensureRootPath(root, plugin.RealPath); err != nil {
		root.Close()
		return nil, fmt.Errorf("pinning plugin source: %w", err)
	}
	return root, nil
}

func installDestination(plugin Plugin) string {
	idHash := sha256.Sum256([]byte(plugin.ID))
	return path.Join(installCacheVersion, string(plugin.Dialect), hex.EncodeToString(idHash[:]), plugin.Digest)
}

func installPluginLeaf(plugin Plugin) string {
	if plugin.Dialect == DialectClaude && plugin.Manifest == "" {
		return plugin.Namespace
	}
	return "root"
}

func installMutex(plugin Plugin, cachePath, destinationRel string) *sync.Mutex {
	key := sha256.Sum256([]byte(cachePath + "\x00" + destinationRel + "\x00" + plugin.ID))
	return &installMutexes[int(key[0])%len(installMutexes)]
}

func acquireInstallLock(ctx context.Context, root *os.Root, rel string) error {
	deadline := time.Now().Add(installLockWait)
	for {
		err := createPrivateInstallDirectory(root, rel)
		if err == nil {
			return nil
		}
		if !os.IsExist(err) {
			return fmt.Errorf("acquiring plugin install lock: %w", err)
		}
		info, inspectErr := safeInfo(root, rel)
		if inspectErr != nil {
			return fmt.Errorf("unsafe plugin install lock: %w", inspectErr)
		}
		if info == nil {
			continue
		}
		if !info.IsDir() {
			return errors.New("plugin install lock is not a directory")
		}
		if err := validatePrivateInstallDirectory(root, rel, info); err != nil {
			return fmt.Errorf("plugin install lock is not private: %w", err)
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for another plugin installation")
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func makeInstallStage(root *os.Root, parentRel string) (string, error) {
	for range 100 {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", fmt.Errorf("creating staged plugin name: %w", err)
		}
		rel := path.Join(parentRel, ".install-"+hex.EncodeToString(nonce[:]))
		if err := createPrivateInstallDirectory(root, rel); err == nil {
			return rel, nil
		} else if !os.IsExist(err) {
			return "", fmt.Errorf("creating staged plugin root: %w", err)
		}
	}
	return "", errors.New("could not allocate a unique staged plugin root")
}

func copyInstallDirectory(source, destination *os.Root, prefix string, depth int, budget *digestBudget) error {
	return copyInstallDirectoryContext(context.Background(), source, destination, prefix, depth, budget)
}

func copyInstallDirectoryContext(ctx context.Context, source, destination *os.Root, prefix string, depth int, budget *digestBudget) error {
	entries, err := readDigestDirectory(source, ".", maxDigestEntries-budget.entries)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		rel := name
		if prefix != "" {
			rel = path.Join(prefix, name)
		}
		if _, err := safeRelativePath(rel); err != nil {
			return fmt.Errorf("unsafe plugin path %q: %w", rel, err)
		}
		if name == ".git" {
			if err := validateExcludedGit(source, name); err != nil {
				return err
			}
			continue
		}
		if depth+1 > maxDigestDepth {
			return fmt.Errorf("plugin path exceeds %d-directory depth limit: %q", maxDigestDepth, rel)
		}
		info, err := safeInfo(source, name)
		if err != nil {
			return err
		}
		if info == nil {
			return fmt.Errorf("plugin path disappeared during copy: %q", rel)
		}
		budget.entries++
		if budget.entries > maxDigestEntries {
			return fmt.Errorf("plugin has more than %d filesystem entries", maxDigestEntries)
		}
		switch {
		case info.Mode().IsRegular():
			written, err := copyInstallFileContext(ctx, source, destination, name, rel, info, int64(maxDigestBytes)-budget.bytes)
			if err != nil {
				return err
			}
			budget.bytes += written
		case info.IsDir():
			if err := createPrivateInstallDirectory(destination, name); err != nil {
				return err
			}
			sourceChild, err := openInstallSubdirectory(source, name)
			if err != nil {
				return err
			}
			childInfo, err := sourceChild.Stat(".")
			if err != nil || !os.SameFile(info, childInfo) {
				sourceChild.Close()
				if err != nil {
					return err
				}
				return fmt.Errorf("plugin directory changed during copy: %q", rel)
			}
			destinationChild, err := openInstallSubdirectory(destination, name)
			if err != nil {
				sourceChild.Close()
				return err
			}
			copyErr := copyInstallDirectoryContext(ctx, sourceChild, destinationChild, rel, depth+1, budget)
			destinationCloseErr := destinationChild.Close()
			sourceCloseErr := sourceChild.Close()
			if copyErr != nil {
				return copyErr
			}
			if destinationCloseErr != nil {
				return destinationCloseErr
			}
			if sourceCloseErr != nil {
				return sourceCloseErr
			}
		default:
			return fmt.Errorf("special file %q is not allowed", rel)
		}
	}
	return nil
}

func copyInstallFile(source, destination *os.Root, name, rel string, expected os.FileInfo, remaining int64) (int64, error) {
	return copyInstallFileContext(context.Background(), source, destination, name, rel, expected, remaining)
}

func copyInstallFileContext(ctx context.Context, source, destination *os.Root, name, rel string, expected os.FileInfo, remaining int64) (int64, error) {
	return copyInstallFileContextWithHook(ctx, source, destination, name, rel, expected, remaining, nil)
}

func copyInstallFileContextWithHook(ctx context.Context, source, destination *os.Root, name, rel string, expected os.FileInfo, remaining int64, beforeOpen func()) (int64, error) {
	if remaining < 0 || expected.Size() > remaining {
		return 0, fmt.Errorf("plugin content exceeds %d-byte digest limit", maxDigestBytes)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	input, err := openExtensionRootRead(source, name)
	if err != nil {
		return 0, err
	}
	actual, err := input.Stat()
	if err != nil {
		input.Close()
		return 0, err
	}
	if !actual.Mode().IsRegular() || !os.SameFile(expected, actual) {
		input.Close()
		return 0, fmt.Errorf("plugin file changed during copy: %q", rel)
	}
	executable := installSourceExecutable(actual)
	mode := os.FileMode(0o600)
	if executable {
		mode = 0o700
	}
	output, err := destination.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		input.Close()
		return 0, err
	}
	if err := securePrivateInstallFile(destination, name, output, executable); err != nil {
		output.Close()
		input.Close()
		return 0, err
	}
	written, copyErr := io.Copy(output, io.LimitReader(contextReader{ctx: ctx, reader: input}, remaining+1))
	if copyErr == nil && written > remaining {
		copyErr = fmt.Errorf("plugin content exceeds %d-byte digest limit", maxDigestBytes)
	}
	if copyErr == nil {
		copyErr = securePrivateInstallFile(destination, name, output, executable)
	}
	if copyErr == nil {
		copyErr = output.Sync()
	}
	finished, finishedErr := input.Stat()
	linked, linkedErr := safeInfo(source, name)
	if copyErr == nil && (finishedErr != nil || linkedErr != nil || linked == nil ||
		!finished.Mode().IsRegular() || !linked.Mode().IsRegular() ||
		!os.SameFile(actual, finished) || !os.SameFile(finished, linked) ||
		actual.Size() != finished.Size() || finished.Size() != written ||
		!actual.ModTime().Equal(finished.ModTime())) {
		copyErr = errors.Join(finishedErr, linkedErr, fmt.Errorf("plugin file changed during copy: %q", rel))
	}
	outputCloseErr := output.Close()
	inputCloseErr := input.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	if outputCloseErr != nil {
		return 0, outputCloseErr
	}
	if inputCloseErr != nil {
		return 0, inputCloseErr
	}
	return written, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func inspectInstalled(root *os.Root, cachePath, objectRel string, expected Plugin) (Plugin, bool, error) {
	info, err := safeInfo(root, objectRel)
	if err != nil {
		return Plugin{}, false, fmt.Errorf("unsafe existing install destination: %w", err)
	}
	if info == nil {
		return Plugin{}, false, nil
	}
	if !info.IsDir() {
		return Plugin{}, true, errors.New("existing install destination is not a directory")
	}
	if err := validatePrivateInstallDirectory(root, objectRel, info); err != nil {
		return Plugin{}, true, fmt.Errorf("existing install destination is not private: %w", err)
	}
	objectRoot, err := openInstallSubdirectory(root, filepath.FromSlash(objectRel))
	if err != nil {
		return Plugin{}, true, fmt.Errorf("opening existing install destination: %w", err)
	}
	defer objectRoot.Close()
	entries, err := readDigestDirectory(objectRoot, ".", 2)
	if err != nil {
		return Plugin{}, true, fmt.Errorf("reading existing install object: %w", err)
	}
	leaf := installPluginLeaf(expected)
	if len(entries) != 1 || entries[0].Name() != leaf {
		return Plugin{}, true, fmt.Errorf("existing install object must contain only plugin root %q", leaf)
	}
	leafInfo, err := safeInfo(objectRoot, leaf)
	if err != nil {
		return Plugin{}, true, fmt.Errorf("unsafe installed plugin root: %w", err)
	}
	if leafInfo == nil || !leafInfo.IsDir() {
		return Plugin{}, true, errors.New("installed plugin root is not a directory")
	}
	if err := validatePrivateInstallDirectory(objectRoot, leaf, leafInfo); err != nil {
		return Plugin{}, true, fmt.Errorf("installed plugin root is not private: %w", err)
	}
	installedRoot, err := openInstallSubdirectory(objectRoot, leaf)
	if err != nil {
		return Plugin{}, true, fmt.Errorf("opening installed plugin root: %w", err)
	}
	defer installedRoot.Close()
	if err := validateInstalledProtection(installedRoot, "", 0, &digestBudget{}); err != nil {
		return Plugin{}, true, fmt.Errorf("invalid installed plugin tree: %w", err)
	}
	pluginRel := path.Join(objectRel, leaf)
	installedPath := filepath.Join(cachePath, filepath.FromSlash(pluginRel))
	plugin, err := discoverExactPlugin(installedPath, installedRoot, expected)
	if err != nil {
		return Plugin{}, true, fmt.Errorf("existing install destination does not match %s: %w", expected.ID, err)
	}
	return plugin, true, nil
}

func validateInstalledProtection(root *os.Root, prefix string, depth int, budget *digestBudget) error {
	entries, err := readDigestDirectory(root, ".", maxDigestEntries-budget.entries)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		rel := name
		if prefix != "" {
			rel = path.Join(prefix, name)
		}
		if _, err := safeRelativePath(rel); err != nil {
			return err
		}
		if name == ".git" {
			return fmt.Errorf("excluded .git path is present in installed object: %q", rel)
		}
		if depth+1 > maxDigestDepth {
			return fmt.Errorf("plugin path exceeds %d-directory depth limit: %q", maxDigestDepth, rel)
		}
		info, err := safeInfo(root, name)
		if err != nil {
			return err
		}
		if info == nil {
			return fmt.Errorf("installed plugin path disappeared: %q", rel)
		}
		budget.entries++
		if budget.entries > maxDigestEntries {
			return fmt.Errorf("plugin has more than %d filesystem entries", maxDigestEntries)
		}
		switch {
		case info.Mode().IsRegular():
			if err := validatePrivateInstallFile(root, name, info, installSourceExecutable(info)); err != nil {
				return fmt.Errorf("installed file %q is not private: %w", rel, err)
			}
		case info.IsDir():
			if err := validatePrivateInstallDirectory(root, name, info); err != nil {
				return fmt.Errorf("installed directory %q is not private: %w", rel, err)
			}
			child, err := openInstallSubdirectory(root, name)
			if err != nil {
				return err
			}
			childInfo, err := child.Stat(".")
			if err != nil || !os.SameFile(info, childInfo) {
				child.Close()
				if err != nil {
					return err
				}
				return fmt.Errorf("installed directory changed during validation: %q", rel)
			}
			validateErr := validateInstalledProtection(child, rel, depth+1, budget)
			closeErr := child.Close()
			if validateErr != nil {
				return validateErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("special file %q is not allowed", rel)
		}
	}
	return nil
}

func inspectExactPlugin(pluginPath string, root *os.Root, expected Plugin, label string) error {
	_, err := discoverExactPlugin(pluginPath, root, expected)
	if err != nil {
		return fmt.Errorf("%s does not match discovered plugin %s: %w", label, expected.ID, err)
	}
	return nil
}

func discoverExactPlugin(pluginPath string, root *os.Root, expected Plugin) (Plugin, error) {
	if err := ensureRootPath(root, pluginPath); err != nil {
		return Plugin{}, err
	}
	result := Discover([]Candidate{{Root: pluginPath, Scope: expected.Scope, Dialect: expected.Dialect}})
	if err := ensureRootPath(root, pluginPath); err != nil {
		return Plugin{}, err
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Severity == SeverityError {
			return Plugin{}, fmt.Errorf("discovery %s: %s", diagnostic.Code, diagnostic.Message)
		}
	}
	if len(result.Plugins) != 1 {
		return Plugin{}, fmt.Errorf("discovery returned %d plugins", len(result.Plugins))
	}
	installed := result.Plugins[0]
	if installed.ID != expected.ID {
		return Plugin{}, fmt.Errorf("ID changed from %q to %q", expected.ID, installed.ID)
	}
	if installed.Dialect != expected.Dialect {
		return Plugin{}, fmt.Errorf("dialect changed from %q to %q", expected.Dialect, installed.Dialect)
	}
	if installed.Digest != expected.Digest {
		return Plugin{}, fmt.Errorf("digest changed from %q to %q", expected.Digest, installed.Digest)
	}
	if err := comparePluginSemantics(expected, installed); err != nil {
		return Plugin{}, err
	}
	return installed, nil
}

func comparePluginSemantics(expected, actual Plugin) error {
	if actual.Kind != expected.Kind || actual.Scope != expected.Scope || actual.Namespace != expected.Namespace {
		return errors.New("normalized plugin identity changed")
	}
	if (actual.Manifest == "") != (expected.Manifest == "") {
		return errors.New("plugin manifest presence changed")
	}
	if actual.Executable != expected.Executable {
		return errors.New("plugin executable classification changed")
	}
	if len(actual.Components) != len(expected.Components) {
		return fmt.Errorf("plugin component count changed from %d to %d", len(expected.Components), len(actual.Components))
	}
	for index := range expected.Components {
		want := expected.Components[index]
		got := actual.Components[index]
		if want.Kind != got.Kind || want.Source != got.Source || want.DeclaredPath != got.DeclaredPath ||
			want.Inline != got.Inline || want.Executable != got.Executable {
			return fmt.Errorf("plugin component %d semantics changed", index)
		}
		wantRel, err := componentRelativePath(expected, want)
		if err != nil {
			return fmt.Errorf("invalid discovered component %d: %w", index, err)
		}
		gotRel, err := componentRelativePath(actual, got)
		if err != nil {
			return fmt.Errorf("invalid installed component %d: %w", index, err)
		}
		if wantRel != gotRel {
			return fmt.Errorf("plugin component %d path changed from %q to %q", index, wantRel, gotRel)
		}
	}
	return nil
}

func componentRelativePath(plugin Plugin, component Component) (string, error) {
	if component.Inline {
		return "inline", nil
	}
	rel, err := filepath.Rel(plugin.RealPath, component.RealPath)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return rel, nil
	}
	return safeRelativePath(rel)
}

func ensureRootPath(root *os.Root, rootPath string) error {
	anchored, err := root.Stat(".")
	if err != nil {
		return err
	}
	current, err := os.Stat(rootPath)
	if err != nil {
		return err
	}
	if !os.SameFile(anchored, current) {
		return errors.New("root path was replaced or retargeted")
	}
	return nil
}

// os.OpenRoot and Root.OpenRoot open their final path component before they
// verify that it is a directory. On Unix, opening a FIFO that way can block.
// Appending a literal directory-self component makes the caller-controlled
// component an intermediate directory lookup, so the kernel rejects a FIFO
// (or another non-directory) without opening it.
func openInstallDirectory(name string) (*os.Root, error) {
	return rootedfs.OpenRoot(name)
}

func openInstallSubdirectory(root *os.Root, name string) (*os.Root, error) {
	return rootedfs.OpenRootAt(root, name)
}
