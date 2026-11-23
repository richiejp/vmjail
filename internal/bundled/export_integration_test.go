//go:build linux && integration

package bundled

// LoadAssetsForTest exposes sealing to the external real-VM test without
// adding a caller-supplied asset-loading API to production builds.
func LoadAssetsForTest(vmm, kernel, initramfs, virtiofsd []byte) (Paths, error) {
	return loadAssets(vmm, kernel, initramfs, virtiofsd)
}
