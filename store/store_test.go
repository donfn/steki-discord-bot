package store

import (
	"testing"
)

func openTestRegistry(t *testing.T) *Registry { return OpenTestRegistry(t) }
func openTestGuard(t *testing.T) *Guard       { return OpenTestGuard(t) }
func openTestActivity(t *testing.T) *Activity { return OpenTestActivity(t) }
func openTestLibrary(t *testing.T) *Library   { return OpenTestLibrary(t) }
