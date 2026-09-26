//go:build providerkind_fixture

package fixtures

// FixtureUnmarkedMock is the required negative control: a type whose name
// matches the mock-name heuristic but that carries no SyntheticComponent
// method. ScanForUnmarkedMocks must report it - proving a mock with the
// marker "deleted" (never added, here) goes red.
type FixtureUnmarkedMock struct{}
