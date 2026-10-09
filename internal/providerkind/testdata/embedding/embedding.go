//go:build providerkind_fixture

// Package embedding holds ScanForSyntheticEmbedding's own negative-control fixtures (B13-B).
// Under testdata/ so the go tool never compiles it; the scan parses it directly.
package embedding

import "github.com/Diansalas/igaming-platform/internal/providerkind"

// FixtureMarker is a Synthetic-marked type.
type FixtureMarker struct{}

func (FixtureMarker) SyntheticComponent() {}

// RealEmbedsMarker is a "real" type that embeds the marked type: FINDING.
type RealEmbedsMarker struct {
	FixtureMarker
	Name string
}

// RealEmbedsPtr embeds a pointer to the marked type: FINDING.
type RealEmbedsPtr struct{ *FixtureMarker }

// RealEmbedsChain embeds a type that itself inherited the marker: FINDING (transitive).
type RealEmbedsChain struct{ RealEmbedsMarker }

// RealEmbedsInterface embeds the providerkind.Synthetic interface: FINDING.
type RealEmbedsInterface struct{ providerkind.Synthetic }

// OwnMarker embeds the marked type but declares its OWN marker: deliberately a mock, NOT a finding.
type OwnMarker struct{ FixtureMarker }

func (OwnMarker) SyntheticComponent() {}

// Composes holds the marked type in a NAMED field: composition, NOT a finding.
type Composes struct{ M FixtureMarker }

// Plain is unrelated: NOT a finding.
type Plain struct{ X int }
