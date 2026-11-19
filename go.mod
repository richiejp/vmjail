module github.com/richiejp/vmjail

go 1.25.0

require (
	connectrpc.com/connect v1.19.2
	github.com/cilium/ebpf v0.21.0
	github.com/containers/gvisor-tap-vsock v0.8.9
	github.com/dgraph-io/badger/v4 v4.9.1
	github.com/go-webauthn/webauthn v0.17.4
	github.com/google/go-containerregistry v0.21.5
	github.com/hanwen/go-fuse/v2 v2.10.1
	github.com/inetaf/tcpproxy v0.0.0-20250222171855-c4b9df066048
	github.com/klauspost/compress v1.18.5
	github.com/mdlayher/vsock v1.2.1
	github.com/modelcontextprotocol/go-sdk v1.4.1
	github.com/spf13/cobra v1.10.2
	github.com/vishvananda/netlink v1.3.1
	github.com/xeipuuv/gojsonschema v1.2.0
	golang.org/x/crypto v0.52.0
	golang.org/x/net v0.54.0
	golang.org/x/sync v0.20.0
	golang.org/x/sys v0.45.0
	golang.org/x/term v0.43.0
	golang.org/x/tools v0.44.0
	google.golang.org/protobuf v1.36.11
	gvisor.dev/gvisor v0.0.0-20240916094835-a174eb65023f
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/apparentlymart/go-cidr v1.1.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/stargz-snapshotter/estargz v0.18.2 // indirect
	github.com/dgraph-io/ristretto/v2 v2.2.0 // indirect
	github.com/docker/cli v29.4.0+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fxamacker/cbor/v2 v2.9.2 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/go-webauthn/x v0.2.6 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/google/flatbuffers v25.2.10+incompatible // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/insomniacslk/dhcp v0.0.0-20240710054256-ddd8a41251c9 // indirect
	github.com/mdlayher/socket v0.5.1 // indirect
	github.com/miekg/dns v1.1.72 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.14 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/u-root/uio v0.0.0-20240224005618-d2acac8f3701 // indirect
	github.com/vbatts/tar-split v0.12.2 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	github.com/xeipuuv/gojsonpointer v0.0.0-20180127040702-4e3ac2762d5f // indirect
	github.com/xeipuuv/gojsonreference v0.0.0-20180127040603-bd5ef7bd5415 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/otel v1.37.0 // indirect
	go.opentelemetry.io/otel/metric v1.37.0 // indirect
	go.opentelemetry.io/otel/trace v1.37.0 // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	golang.org/x/time v0.5.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
)

tool (
	connectrpc.com/connect/cmd/protoc-gen-connect-go
	github.com/cilium/ebpf/cmd/bpf2go
	google.golang.org/protobuf/cmd/protoc-gen-go
)
