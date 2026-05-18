module github.com/richiejp/darkaifactory

go 1.25.0

require (
	connectrpc.com/connect v1.19.2
	github.com/google/go-containerregistry v0.21.5
	github.com/hanwen/go-fuse/v2 v2.10.1
	github.com/klauspost/compress v1.18.5
	github.com/mdlayher/vsock v1.2.1
	github.com/spf13/cobra v1.10.2
	github.com/xeipuuv/gojsonschema v1.2.0
	golang.org/x/net v0.54.0
	golang.org/x/sync v0.20.0
	golang.org/x/sys v0.44.0
	golang.org/x/term v0.43.0
	golang.org/x/tools v0.44.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/containerd/stargz-snapshotter/estargz v0.18.2 // indirect
	github.com/docker/cli v29.4.0+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mdlayher/socket v0.4.1 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/vbatts/tar-split v0.12.2 // indirect
	github.com/xeipuuv/gojsonpointer v0.0.0-20180127040702-4e3ac2762d5f // indirect
	github.com/xeipuuv/gojsonreference v0.0.0-20180127040603-bd5ef7bd5415 // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
)

tool (
	connectrpc.com/connect/cmd/protoc-gen-connect-go
	google.golang.org/protobuf/cmd/protoc-gen-go
)
