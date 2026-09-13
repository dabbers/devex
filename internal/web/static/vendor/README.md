# Vendored assets

These are committed rather than fetched at runtime so the control plane works
on a machine with no route to the internet, which a self-hosted deployment may
well be. They are also embedded in the binary, so there is nothing to install.

| File | Source | Version | Licence |
| --- | --- | --- | --- |
| `xterm.js` | https://cdn.jsdelivr.net/npm/xterm@5.3.0/lib/xterm.js | 5.3.0 | MIT |
| `xterm.css` | https://cdn.jsdelivr.net/npm/xterm@5.3.0/css/xterm.css | 5.3.0 | MIT |
| `xterm-addon-fit.js` | https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.8.0/lib/xterm-addon-fit.js | 0.8.0 | MIT |

xterm.js renders the workspace shell. A terminal emulator is not something to
hand-roll: escape sequences, cursor addressing, scrollback and selection are
exactly the kind of detail that looks simple and is not.

To update, re-fetch at the pinned URL and run the tests; `internal/web` asserts
these files are embedded and non-trivial.
