# Terminal browser dependency

`@xterm/xterm` is pinned exactly. Bun lifecycle scripts are disabled both in
`package.json` and `bunfig.toml`; dependency updates must use
`bun install --ignore-scripts` inside a darkaifactory Sandbox. The published
tarball for 6.0.0 must have SHA-512:

```
4d0c037501adc300edfb67202832e7d0845a4b162ed6d4948e029aad20e450cd193624915c5a63c44b2f73f66073992ae68989f95d1af3f908336583dec30e62
```

Only `lib/xterm.js`, `css/xterm.css`, and the MIT license are copied into the
Go-embedded assets. No package code is executed while producing those files.
Run `scripts/update-terminal-assets.sh` through `daft workspace exec` with
`DAFT_SANDBOX_BUILD=1`; the script refuses a normal host invocation and checks
the content hash of every copied file.
