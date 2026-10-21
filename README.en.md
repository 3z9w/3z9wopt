# 3z9wOpt — Windows Optimization Toolbox

> 无名大叔系统优化工具箱 · an open-source Windows toolbox written in Go (Wails v2 + WebView2).
> It cleans junk files, tunes the system, manages startup items and drivers, analyses disk usage,
> reads BSOD crash dumps, and runs 34 built-in system tools.
>
> **Everything is read from your real machine — and when a value can't be read, it says so instead of making one up.**
> No telemetry, no bundled adware, every change is logged and reversible.

- Status: v1.1.0 · actively developed
- Platform: Windows 10 22H2 / 11 (x64) + WebView2 Runtime
- License: [GPLv3](LICENSE) — you may use, modify and distribute it freely, but if you distribute a modified version you must release your source under GPLv3 as well
- Website: <https://www.3z9w.com>
- Download page: <https://opt.3z9w.com>
- Source: <https://github.com/3z9w/3z9wopt>
- 中文说明：[README.md](README.md)

## Features

| Module | What it does |
|---|---|
| Junk cleanup | 93 targets / 9 groups, path-level de-duplication, scan is read-only |
| System tuning | 154 tweaks (basic 87 / advanced 127), **each one individually reversible** |
| App uninstall | Desktop + UWP apps, runs the vendor uninstaller and **re-enumerates to confirm** |
| Disk usage | Reads the NTFS MFT index directly, drills down by real subtree size |
| Startup items | Enumerated the Windows way: registry Run / `StartupApproved` / services / scheduled tasks / startup folder. Third-party entries only by default; disabling writes the system approval flag instead of deleting keys, so it is always reversible |
| Driver store | Export, reinstall and delete third-party driver packages (**no version guessing**) |
| BSOD analysis | Parses Minidump / MEMORY.DMP: bug check code, parameters, cause, advice, suspect drivers |
| Toolbox | 34 system tools (SFC / DISM / network repair / boot repair …) — **really executed, live output, stoppable** |
| Guided troubleshooting | Pick your symptom, get steps, one-click run the right tool |
| Hardware info | CPUID / SMBIOS / WMI / DXGI: CPU cache & codename, memory sticks, GPU, board, BIOS |

## Design principles

1. **No made-up data** — unreadable values are shown as "读不到 / unavailable", never estimated.
2. **No guessing** — e.g. it does *not* claim "3 drivers can be updated" (that would require checking every vendor site).
3. **Everything is logged and reversible** — operation log, per-item restore, one-click revert.
4. **Dangerous actions use a three-part confirmation** — what will happen / how wide the impact / how to undo.
5. **No telemetry** — nothing is reported anywhere. Only two things ever touch the network, and both are manual: the built-in speed test, and "Check for updates" (one request to the download page, result shown in the window).
6. **First screen fits** — the home page needs no scrolling.

## Screenshots

| Home | Junk cleanup |
|---|---|
| ![Home](docs/img/shot-home.jpg) | ![Junk cleanup](docs/img/shot-clean.jpg) |
| System tuning | Startup items |
| ![System tuning](docs/img/shot-optimize.jpg) | ![Startup items](docs/img/shot-startup.jpg) |
| Disk usage | Hardware info |
| ![Disk usage](docs/img/shot-space.jpg) | ![Hardware info](docs/img/shot-pcinfo.jpg) |

## Download & build

Releases are published on [GitHub](https://github.com/3z9w/3z9wopt/releases) (exe attached to each Release);
the official download page is <https://opt.3z9w.com>.

Build from source (Go 1.26+, network needed — no `vendor/` directory):

```powershell
go build -tags desktop,production -ldflags "-s -w -H windowsgui -X main.appVersion=1.1.0 -X main.buildDate=2026-10-16" -o bin\3z9wOpt_v1.1.0.exe .
```

The UI lives entirely in `design/ui-prototype.html` (single file, inline CSS/JS — no frontend build chain).
Version and build date are injected at compile time, so a fresh clone shows its own values.

## Disclaimer

This tool **modifies system settings** (service start types, registry values, scheduled tasks, system parameters).
It ships with an operation log and restore support, but please **create a system restore point first**
(the toolbox has a one-click entry for that). Use at your own risk — see [LICENSE](LICENSE).

## Third-party

See [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md) (10 modules, all MIT / BSD licensed),
and [Wails](https://wails.io/) + Microsoft Edge WebView2.
