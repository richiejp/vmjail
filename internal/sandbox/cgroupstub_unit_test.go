//go:build !integration

package sandbox

// cgroupStubInUnitTests gates the TestMain cgroup stub. In the unit
// build (no integration tag) it is true: the package's boot tests use
// fake VMM runners and run on hosts without cgroup delegation, so the
// per-VM cgroup constructor is stubbed to a no-op. The real
// createVMCgroup is still covered directly by the TestCreateVMCgroup_*
// tests and end-to-end by the (delegated) integration build, where this
// const is false.
const cgroupStubInUnitTests = true
