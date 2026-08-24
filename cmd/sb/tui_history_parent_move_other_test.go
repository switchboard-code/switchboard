//go:build !windows

package main

func historyParentMoveBlockedByRetainedHandle(error) bool { return false }

func historySubstitutionRefusesBeforePublication() bool { return false }
