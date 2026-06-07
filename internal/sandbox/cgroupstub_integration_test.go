//go:build integration

package sandbox

// cgroupStubInUnitTests is false in the integration build: integration
// tests boot real VMs under a delegated cgroup (the make integration
// systemd-run wrap), so they exercise the real createVMCgroup hard
// precondition rather than the unit-test stub. See the unit-build
// counterpart in cgroupstub_unit_test.go.
const cgroupStubInUnitTests = false
