# Troubleshooting

## `tracebloc: command not found` after install

The installer downloads a single binary and drops it in a `bin` directory. If
that directory isn't on your shell's `PATH`, the `tracebloc` command won't be
found even though the binary is installed.

### Why it happens

`install.sh` installs to `/usr/local/bin` when that's writable. The usual
unprivileged one-liner (`curl … | sh`) **can't** write there, so it falls back
to **`$HOME/.local/bin`** — which isn't on `PATH` in many setups.

The installer adds `$HOME/.local/bin` to the rc file your shell actually reads
(`~/.bashrc`, `~/.zshrc`, `~/.bash_profile` on macOS, `~/.config/fish/config.fish`,
or `~/.profile`), but a shell that's **already running** won't see the change —
you need a new shell, or to re-load the rc file.

On Linux desktops there's an extra trap: the stock `~/.profile` only adds
`~/.local/bin` to `PATH` **at login**, and only if the directory already existed
at login time. The installer creates it mid-session, and a new terminal window
is an interactive *non-login* shell that reads `~/.bashrc` (not `~/.profile`) —
so "open a new terminal" alone never triggers the `~/.profile` logic. That's why
the installer writes to `~/.bashrc` directly.

### Fix it

**1. Confirm it's only a PATH problem** — run the binary by its full path:

```sh
~/.local/bin/tracebloc version   # or: /usr/local/bin/tracebloc version
```

If that prints a version, the install is fine and this is purely `PATH`.

**2. Pick up the PATH entry the installer added** — open a **new** terminal, or
re-load your rc file in the current one:

```sh
. ~/.bashrc        # bash on Linux
. ~/.zshrc         # zsh (macOS default)
. ~/.bash_profile  # bash on macOS
```

Then:

```sh
tracebloc version
```

**3. If it's still not found**, check which shell you're in and that the entry
landed in the matching rc file:

```sh
echo "$SHELL"
grep -n '.local/bin' ~/.bashrc ~/.zshrc ~/.bash_profile ~/.profile 2>/dev/null
```

If the line is in the wrong file (e.g. `~/.bashrc` while you actually run zsh, or
`~/.bashrc` on macOS where Terminal opens a login shell that reads
`~/.bash_profile`), add it to the right one:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc   # adjust file to your shell
. ~/.zshrc
```

### Cleanest alternative: install where PATH already points

`/usr/local/bin` is on `PATH` for both login and non-login shells out of the box
on Linux and macOS. Installing there sidesteps the whole rc/login-shell question:

```sh
# Move the binary you already have:
sudo mv ~/.local/bin/tracebloc /usr/local/bin/

# …or re-run the installer with write access to /usr/local/bin:
curl -fsSL https://github.com/tracebloc/cli/releases/latest/download/install.sh | sudo sh
```

### Windows (PowerShell)

`install.ps1` installs to `%LOCALAPPDATA%\Programs\tracebloc` and adds it to your
**user** `PATH` automatically. The current PowerShell session won't refresh —
**open a new PowerShell window**, then:

```powershell
tracebloc version
```

If it's still missing, confirm the entry and check nothing at Process/Machine
scope overrides it:

```powershell
[Environment]::GetEnvironmentVariable('Path','User') -split ';' | Select-String tracebloc
```

## Server / SSH sessions

Each SSH login is a *login* shell, so it reads `~/.profile` (or `~/.bash_profile`
/ `~/.bash_login` if one exists — bash reads only the **first** that's present).
If you have a `~/.bash_profile` that doesn't source `~/.profile` or `~/.bashrc`,
the PATH entry may be skipped. Either add the `export PATH=…` line to the file
your login shell actually reads, or use the `/usr/local/bin` approach above.

## Exit codes

Every command exits `0` on success. Non-zero codes are a scripting contract —
they're stable, and each command's `--help` documents the subset it can
produce (`tracebloc data ingest --help` has the fullest list). This is the
cross-command view; the names in the last column are the constants in
`internal/cli/exitcodes.go`, so grepping a name finds every site that
produces that code.

| Code | Meaning | Produced by | Constant |
|------|---------|-------------|----------|
| `0` | Success — includes `--dry-run` completing, a guided run you cancelled cleanly, and `doctor` passing with warnings only | all commands | `exitOK` |
| `1` | Generic failure with no more specific bucket (also any error without an explicit code) | `login`, `client …`, `delete`, mistyped commands | `exitFailure` |
| `2` | Your input didn't validate: schema validation failed (spec synthesized from flags, or your YAML), an unsupported/unknown `--task`, a task-scoped flag applied to the wrong task, an invalid dataset name, or a resource size that doesn't fit the machine | `data ingest`, `data validate`, `data delete`, `resources set` | `exitBadInput` |
| `2` | One or more checks failed | `doctor` | `exitChecksFailed` |
| `3` | Local environment problem: kubeconfig couldn't be loaded, the dataset path is missing or unreadable, the local layout is wrong, a YAML file didn't parse, or a prompt was needed but the run is non-interactive (`--no-input` / `--output-json` / no TTY) | `data ingest`, `data validate`, `data list`, `data delete`, `doctor`, `resources`, `resources set` | `exitLocalEnv` |
| `4` | Cluster reachable but no tracebloc client found in the namespace — or its shared storage / dataset list is missing, so the target can't be confirmed | `data ingest`, `data list`, `data delete`, `cluster info`, `resources`, `resources set` | `exitNoWorkspace` |
| `5` | Auth: the ingestor SA token couldn't be obtained, or jobs-manager rejected it (401/403) | `data ingest`, `cluster info` | `exitAuth` |
| `5` | No dataset by that name on this client (nothing to delete) | `data delete` | `exitNoSuchDataset` |
| `6` | Destination table already exists — re-run with `--overwrite` to replace it, or pick a different `--name` | `data ingest` | `exitTableExists` |
| `7` | Pre-flight succeeded but staging the files failed (Pod creation, image pull, exec stream, or remote tar error) | `data ingest` | `exitStagingFailed` |
| `7` | Removing an existing table + its files failed partway (see the error for the recovery command) | `data delete`, `data ingest --overwrite` | `exitTeardownFailed` |
| `7` | The cluster couldn't be queried for its datasets | `data list` | `exitQueryFailed` |
| `8` | jobs-manager rejected the submitted run (a non-auth 4xx/5xx), or the port-forward to it couldn't be set up | `data ingest` | `exitSubmitFailed` |
| `9` | The ingestion Job exited non-zero, completed with row-level failures the summary panel reports, or its outcome couldn't be determined / followed | `data ingest` | `exitIngestFailed` |
| `130` | You hit Ctrl-C at an interactive prompt (128+SIGINT) | interactive prompts | `exitInterrupted` |

## Still stuck?

Open an issue at [github.com/tracebloc/cli/issues](https://github.com/tracebloc/cli/issues)
with your OS, shell (`echo "$SHELL"`), and the output of `ls -l ~/.local/bin/tracebloc`.
