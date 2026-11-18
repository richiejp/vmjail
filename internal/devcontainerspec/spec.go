package devcontainerspec

// Spec contains local container launch settings derived from a devcontainer
// configuration and explicit overrides. Build inputs remain on devcontainer.Spec.
type Spec struct {
	ImageRef             string
	Mounts               []Mount
	Env                  []EnvVar
	Ports                []PortMapping
	MemoryBytes          uint64
	CPUCores             uint32
	GpuCount             uint32
	GpuMinVRAM           uint64
	Isolation            int32
	NestedVirt           bool
	PrivilegedFuseMounts bool
	PrivilegedBuild      bool
}

// Mount shares a host source directory at a container target path.
type Mount struct {
	Target   string
	Source   string
	ReadOnly bool
}

// EnvVar is a container environment variable.
type EnvVar struct {
	Name  string
	Value string
}

// PortMapping identifies a container port. Protocol uses the sandbox wire
// values (TCP=1, UDP=2) until the local launch interface replaces that protocol.
type PortMapping struct {
	ContainerPort uint32
	Protocol      int32
}
