package catalog

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

//go:embed bundled.toml
var bundledTOML string

// UserOverrideFile is where a user corrects an entry or describes a local
// endpoint the bundled catalog cannot know about.
const UserOverrideFile = "models.toml"

const maxUserOverrideBytes = 1 << 20

type catalogFile struct {
	Revision       string      `toml:"revision"`
	Model          []ModelInfo `toml:"model"`
	SurfaceDefault []ModelInfo `toml:"surface_default"`
}

// Load reads the bundled catalog and layers the user's overrides on top.
//
// Source precedence follows §4: user overrides win, then a runtime probe,
// then a signed remote refresh, then the bundled data. Only the first and
// last exist today; the middle two arrive with their own machinery, and the
// merge is written so adding a layer does not change the callers.
func Load() (*Catalog, error) {
	c, err := loadBundled()
	if err != nil {
		return nil, err
	}

	path, err := userOverridePath()
	if err != nil {
		// No home directory is not a reason to refuse to run; it just means
		// there are no overrides to apply.
		return c, nil
	}
	if err := c.applyOverrides(path); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadBundled reads only the vendored catalog. Tests and the exit-gate
// measurement use it so a stray file in the user's home directory cannot
// change a recorded result.
func LoadBundled() (*Catalog, error) { return loadBundled() }

func loadBundled() (*Catalog, error) {
	var f catalogFile
	if _, err := toml.Decode(bundledTOML, &f); err != nil {
		return nil, fmt.Errorf("parsing the bundled catalog: %w", err)
	}

	c := &Catalog{
		Revision: f.Revision,
		Source:   "bundled",
		entries:  map[string]ModelInfo{},
		defaults: map[string]ModelInfo{},
	}
	if c.Revision == "" {
		return nil, errors.New("the bundled catalog has no revision; a cost recorded against it could not be reproduced")
	}
	// The revision alone does not prove which bytes produced a number, so the
	// content is fingerprinted too.
	sum := sha256.Sum256([]byte(bundledTOML))
	c.Revision += "+" + hex.EncodeToString(sum[:4])

	for _, m := range f.Model {
		if err := validate(m); err != nil {
			return nil, err
		}
		c.entries[m.ID()] = m
	}
	for _, d := range f.SurfaceDefault {
		if err := validateSurfaceDefault(d); err != nil {
			return nil, err
		}
		c.defaults[d.Provider+"/"+d.Surface] = d
	}
	return c, nil
}

func (c *Catalog) applyOverrides(path string) error {
	data, err := rootedfs.ReadFile(filepath.Dir(path), filepath.Base(path), maxUserOverrideBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", path, err)
	}

	var f catalogFile
	if _, err := toml.Decode(string(data), &f); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}

	changed := 0
	for _, m := range f.Model {
		if err := validate(m); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		c.entries[m.ID()] = m
		changed++
	}
	for _, d := range f.SurfaceDefault {
		if err := validateSurfaceDefault(d); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		c.defaults[d.Provider+"/"+d.Surface] = d
		changed++
	}

	if changed > 0 {
		// The revision has to record that local edits are in play, or a cost
		// reconstructed from a session record would be checked against the
		// wrong data.
		sum := sha256.Sum256(data)
		c.Revision += "+user." + hex.EncodeToString(sum[:4])
		c.Source = "bundled+user"
	}
	return nil
}

func userOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".switchboard", UserOverrideFile), nil
}

func validate(m ModelInfo) error {
	switch {
	case m.Provider == "":
		return errors.New("catalog entry has no provider")
	case m.Surface == "":
		return fmt.Errorf("catalog entry %s has no surface", m.ProviderModelID)
	case m.ProviderModelID == "":
		return fmt.Errorf("catalog entry for %s/%s has no provider_model_id", m.Provider, m.Surface)
	case len(m.Pricing) == 0:
		// An entry with no price is worse than a missing entry: it silently
		// prices every request at zero.
		return fmt.Errorf("catalog entry %s has no pricing", m.ID())
	case m.VerifiedAt.IsZero():
		return fmt.Errorf("catalog entry %s has no verified_at; a price with no date cannot be audited", m.ID())
	}
	return validateRanges(m)
}

func validateSurfaceDefault(m ModelInfo) error {
	switch {
	case m.Provider == "":
		return errors.New("catalog surface default has no provider")
	case m.Surface == "":
		return fmt.Errorf("catalog surface default for %s has no surface", m.Provider)
	case len(m.Pricing) == 0:
		return fmt.Errorf("catalog surface default %s/%s has no pricing", m.Provider, m.Surface)
	case m.VerifiedAt.IsZero():
		return fmt.Errorf("catalog surface default %s/%s has no verified_at; a price with no date cannot be audited", m.Provider, m.Surface)
	}
	return validateRanges(m)
}

func validateRanges(m ModelInfo) error {
	id := m.ID()
	if m.ProviderModelID == "" {
		id = m.Provider + "/" + m.Surface
	}

	switch {
	case m.ContextWindow < 0:
		return fmt.Errorf("catalog entry %s has negative context_window", id)
	case m.MaxOutput < 0:
		return fmt.Errorf("catalog entry %s has negative max_output", id)
	case m.Cache.MinTokens < 0:
		return fmt.Errorf("catalog entry %s has negative cache min_tokens", id)
	case m.Cache.MaxBreakpoints < 0:
		return fmt.Errorf("catalog entry %s has negative cache max_breakpoints", id)
	case m.Cache.LookbackBlocks < 0:
		return fmt.Errorf("catalog entry %s has negative cache lookback_blocks", id)
	}

	for i, band := range m.Pricing {
		if err := validatePriceBand(id, i, m, band); err != nil {
			return err
		}
	}

	if Metering(m.Metering.String()) == PerToken && !m.Free() && m.MaxOutput <= 0 {
		return fmt.Errorf("catalog entry %s is paid per-token but max_output is not positive", id)
	}
	return nil
}

func validatePriceBand(id string, index int, model ModelInfo, band PriceBand) error {
	if band.MaxInputTokens < 0 {
		return fmt.Errorf("catalog entry %s pricing band %d has negative max_input_tokens", id, index)
	}
	rates := []struct {
		name string
		rate Money
	}{
		{"input_per_mtok", band.InputPerMTok},
		{"output_per_mtok", band.OutputPerMTok},
		{"cache_read_per_mtok", band.CacheReadPerMTok},
	}
	for _, item := range rates {
		if item.rate < 0 {
			return fmt.Errorf("catalog entry %s pricing band %d has negative %s", id, index, item.name)
		}
	}
	for ttl, rate := range band.CacheWritePerMTok {
		if rate < 0 {
			return fmt.Errorf("catalog entry %s pricing band %d has negative cache_write_per_mtok for %q", id, index, ttl)
		}
	}

	inputLimit := model.ContextWindow
	if band.MaxInputTokens > 0 && (inputLimit == 0 || band.MaxInputTokens < inputLimit) {
		inputLimit = band.MaxInputTokens
	}
	inputRate := band.InputPerMTok
	if band.CacheReadPerMTok > inputRate {
		inputRate = band.CacheReadPerMTok
	}
	for _, rate := range band.CacheWritePerMTok {
		if rate > inputRate {
			inputRate = rate
		}
	}

	inputCost, inputOK := checkedTokenCost(inputRate, inputLimit)
	outputCost, outputOK := checkedTokenCost(band.OutputPerMTok, model.MaxOutput)
	_, totalOK := checkedMoneyAdd(inputCost, outputCost)
	if !inputOK || !outputOK || !totalOK {
		return fmt.Errorf("catalog entry %s pricing band %d exceeds the representable cost range", id, index)
	}
	return nil
}
