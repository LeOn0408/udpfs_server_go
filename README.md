# udpfs_server_go

A stdlib-only Go port of the Python **UDPFS server over UDPRDMA** used by PS2 Neutrino.
It serves a host directory as a filesystem and/or a disk image as block device
handle 0, speaking the same wire protocol as `udpfs_server.py`.

## Build

Requires Go 1.23 or newer. No external dependencies.

```powershell
cd "D:\ROM\PS2\FOR DEVICE\Neutrino\udpfs_server_go"
go build -o udpfs_server_go.exe .
```

## Run

```powershell
# Filesystem only (default root is D:/ROM/PS2/FOR DEVICE/Games)
.\udpfs_server_go.exe

# Explicit root and block device
.\udpfs_server_go.exe --root-dir "D:\ROM\PS2\FOR DEVICE\Games" --block-device "D:\disk.iso"

# Existing Python GUI config file
.\udpfs_server_go.exe --config "D:\ROM\PS2\FOR DEVICE\Neutrino\udpfs_server\udpfs_config.json"

# Custom discovery port / read-only / verbose
.\udpfs_server_go.exe --port 0xF5F6 --read-only --verbose
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--root-dir`, `-d` | `D:/ROM/PS2/FOR DEVICE/Games` | Directory served as the UDPFS filesystem |
| `--block-device`, `-b` | *(none)* | Disk image / block device exposed as handle 0 |
| `--port`, `-p` | `0xF5F6` (62966) | UDP discovery port; accepts hex (`0x...`) or decimal |
| `--read-only`, `-r` | `false` | Reject all writes and mutating operations |
| `--config` | *(none)* | Path to a `udpfs_config.json`-compatible JSON file |
| `--bind`, `-i` | all interfaces | IP address for the ephemeral data socket |
| `--sector-size`, `-s` | `512` | Sector size for the block device |
| `--verbose`, `-v` | `false` | Per-request event logging |
| `--enable-compression`, `-c` | `false` | Accepted for compatibility only — see "Known gaps" |

At least one of `--root-dir` or `--block-device` must be non-empty.

### Config file

`--config` reads the same keys as the Python GUI config, for example:

```json
{
  "root_dir": "D:/ROM/PS2/FOR DEVICE/Games",
  "block_device": "",
  "read_only": 0,
  "enable_compression": 0
}
```

`read_only` and `enable_compression` accept `true`/`false` as well as `1`/`0`.
Optional keys `port`, `bind`, `sector_size` and `verbose` are also understood.
Explicit command-line flags override values from the config file.

## Protocol coverage

Implemented and verified against a local UDPRDMA client:

- UDPRDMA headers: `Header` (2 B), `DiscHeader` (4 B), `DataHeader` (4 B),
  little-endian bitfields, 12-bit sequence numbers.
- DISCOVERY -> INFORM on port `0xF5F6`; INFORM advertises the ephemeral data port
  and service id `0xF5F5`.
- DATA receive: piggyback ACK handling, pure ACK/NACK, duplicate detection and
  retransmission, peer-reset on seq 0, immediate ACK before request processing.
- DATA transmit: single-packet FIN replies plus multi-packet UDPRDMA transfers
  with app headers (`hdr_word_count`), 4-byte padding, send window of 8,
  window ACK/NACK, timeout retransmission and 4 retry limit.
- File messages: OPEN, CLOSE, READ, WRITE_REQ, WRITE_DATA, WRITE_DONE, LSEEK,
  DREAD (PS2 dirent layout), GETSTAT, MKDIR, REMOVE, RMDIR, RESULT_REPLY.
- Block messages (UDPBD subset): BREAD, BWRITE_REQ + WRITE_DATA, and the empty-path
  GETSTAT capacity query for handle 0.
- PS2 `iox_stat_t` encoding: `<BBBBBBH` time fields (year = year-1900), 32-bit
  size/hisize split, `FIO_S_IFREG` / `FIO_S_IFDIR` modes.
- Safety: client paths are normalized, symlink-resolved and rejected unless they
  stay inside the configured root directory.
- Live throttled status line and final session/operation/transfer statistics.

## Verification

Built with Go 1.23.5 on Windows. The server was started briefly and bound:

- Discovery socket: `0.0.0.0:62966` (`0xF5F6`)
- Data socket: ephemeral port, printed at startup and advertised in INFORM

Local UDPRDMA smoke tests covered DISCOVERY/INFORM, OPEN, READ (including a
20,000-byte multi-packet transfer with window ACKs), LSEEK, GETSTAT, DOPEN/DREAD,
CLOSE, block device capacity, BREAD and BWRITE.

## Known gaps / differences from the Python server

- **Compressed ISO support is not implemented.** `.zso`, `.cso` and `.chd` files
  are served as ordinary files (raw bytes); there is no transparent decompression,
  no `.iso` name rewriting in DREAD, and no compressed-size reporting in GETSTAT.
  `--enable-compression` prints a warning and has no effect. Implementing this
  would require LZ4/zlib (stdlib zlib is possible) and a CHD reader.
- `ctime` and `atime` are encoded from the host file's modification time; Go's
  portable `os.FileInfo` does not expose creation/access times. `mtime` is correct.
- LSEEK offsets are interpreted as signed 64-bit values. The Python code built the
  offset from two unsigned 32-bit halves, which turned negative seeks into huge
  positive values; the signed form matches the intended PS2 semantics. Positive
  offsets are identical to Python.
- OS errors are mapped to POSIX errno values for the wire reply; a few unusual
  Windows-specific errors fall back to `EIO`.
- The main loop polls both sockets with short deadlines instead of using
  `select(2)`. Functionally equivalent, with slightly higher idle CPU usage.
- No autostart, service installation or git publishing is performed by this port.

## Windows helper batch files

| File | Purpose |
|------|---------|
| `start.bat` | Start the server hidden, remember its PID in `udpfs.pid` |
| `stop.bat` | Stop the process recorded in `udpfs.pid` |
| `restart.bat` | stop.bat + start.bat |
| `install-autostart.bat` | Create a shortcut in the user Startup folder (no admin needed) |
| `config.json` | Default config: root `D:/ROM/PS2/FOR DEVICE/Games`, read-only off |

The batch files do **not** install a Windows service and do not start anything
unless you run them. Autostart is installed only when `install-autostart.bat`
is executed; remove `udpfs_server_go.lnk` from the Startup folder to undo it.
