// Package userns lifts vmjail-sandboxd into its own user namespace
// at startup so it has the host capabilities needed to manage
// bundles, share filesystems via virtiofsd, and orchestrate the
// container's ownership without the binary itself being setuid
// root.
//
// The classic rootless-container pattern: the process forks a
// child with CLONE_NEWUSER, the parent populates the child's
// uid_map and gid_map via the setuid newuidmap/newgidmap
// helpers (writing multi-range maps directly from an
// unprivileged process is forbidden by the kernel), the child
// proceeds with full capabilities inside that namespace, and
// the parent stays around as a thin signal-forwarder + exit-
// code propagator.
//
// Once inside the namespace, the child has CAP_SYS_ADMIN
// (mount_setattr, idmapped mounts), CAP_CHOWN (rewrite bundle
// ownership for virtiofsd consumption), and CAP_SETUID (write
// nested uid_maps for sub-processes like virtiofsd's own
// sandboxing). The kernel still enforces the userns boundary on
// the actual host filesystem — capabilities apply only to ids
// mapped by the userns, so the practical privilege is bounded
// by the calling user's /etc/subuid + /etc/subgid allocation.
package userns
