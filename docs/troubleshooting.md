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

## Still stuck?

Open an issue at [github.com/tracebloc/cli/issues](https://github.com/tracebloc/cli/issues)
with your OS, shell (`echo "$SHELL"`), and the output of `ls -l ~/.local/bin/tracebloc`.
