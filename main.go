package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

const defaultRootDir = "D:/ROM/PS2/FOR DEVICE/Games"

// flexBool accepts JSON booleans as well as the 0/1 integers used by
// udpfs_config.json.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true", "1":
		*b = true
	case "false", "0", "null":
		*b = false
	default:
		var v bool
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*b = flexBool(v)
	}
	return nil
}

type configFile struct {
	RootDir           *string   `json:"root_dir"`
	BlockDevice       *string   `json:"block_device"`
	ReadOnly          *flexBool `json:"read_only"`
	EnableCompression *flexBool `json:"enable_compression"`
	Port              *int      `json:"port"`
	Bind              *string   `json:"bind"`
	SectorSize        *int      `json:"sector_size"`
	Verbose           *flexBool `json:"verbose"`
}

func loadConfig(path string) (configFile, error) {
	var cfg configFile
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("invalid config JSON: %w", err)
	}
	return cfg, nil
}

// baseInt parses flags with strconv base 0, so "0xF5F6" works like Python's
// int(x, 0).
type baseInt struct{ value int }

func (b *baseInt) String() string { return strconv.Itoa(b.value) }
func (b *baseInt) Set(s string) error {
	v, err := strconv.ParseInt(s, 0, 64)
	if err != nil {
		return err
	}
	b.value = int(v)
	return nil
}

type baseIntAlias struct{ p *baseInt }

func (a baseIntAlias) String() string     { return a.p.String() }
func (a baseIntAlias) Set(v string) error { return a.p.Set(v) }

// Alias values let the Python-style short flags write into the same variable.
type stringAlias struct{ p *string }

func (a stringAlias) String() string     { return *a.p }
func (a stringAlias) Set(v string) error { *a.p = v; return nil }

type boolAlias struct{ p *bool }

func (a boolAlias) String() string { return strconv.FormatBool(*a.p) }
func (a boolAlias) Set(v string) error {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return err
	}
	*a.p = b
	return nil
}

// options holds the effective configuration after flags and config file are
// merged. Flags explicitly supplied on the command line win over the config.
type options struct {
	rootDir           string
	blockDevice       string
	port              int
	bindIP            string
	sectorSize        int
	readOnly          bool
	verbose           bool
	enableCompression bool
}

func main() {
	opts := options{
		rootDir:    defaultRootDir,
		port:       UDPFSPort,
		sectorSize: 512,
	}
	port := baseInt{value: UDPFSPort}
	configPath := ""

	fs := flag.NewFlagSet("udpfs_server_go", flag.ExitOnError)
	fs.StringVar(&opts.rootDir, "root-dir", defaultRootDir, "root directory to serve files from")
	fs.StringVar(&opts.blockDevice, "block-device", "", "block device / disk image served as handle 0")
	fs.Var(&port, "port", "UDP discovery port (default 0xF5F6; accepts hex)")
	fs.BoolVar(&opts.readOnly, "read-only", false, "serve in read-only mode")
	fs.StringVar(&configPath, "config", "", "path to JSON config (udpfs_config.json compatible)")
	fs.StringVar(&opts.bindIP, "bind", "", "IP for the data socket (default: all interfaces)")
	fs.IntVar(&opts.sectorSize, "sector-size", 512, "sector size of the block device")
	fs.BoolVar(&opts.verbose, "verbose", false, "verbose per-request output")
	fs.BoolVar(&opts.enableCompression, "enable-compression", false,
		"accepted for compatibility; compressed ISO support is not implemented")

	// Python-compatible short aliases.
	fs.Var(stringAlias{&opts.rootDir}, "d", "alias for --root-dir")
	fs.Var(stringAlias{&opts.blockDevice}, "b", "alias for --block-device")
	fs.Var(stringAlias{&opts.bindIP}, "i", "alias for --bind")
	fs.Var(baseIntAlias{&port}, "p", "alias for --port")
	fs.Var(boolAlias{&opts.readOnly}, "r", "alias for --read-only")
	fs.Var(boolAlias{&opts.verbose}, "v", "alias for --verbose")
	fs.Var(boolAlias{&opts.enableCompression}, "c", "alias for --enable-compression")
	fs.IntVar(&opts.sectorSize, "s", 512, "alias for --sector-size")

	_ = fs.Parse(os.Args[1:])
	opts.port = port.value

	// Snapshot explicitly supplied flags before merging the config file.
	type cli struct {
		root, block, bind              string
		port, sector                   int
		readOnly, verbose, compression bool
		rootSet, blockSet, bindSet     bool
		portSet, sectorSet             bool
		boolSet, verboseSet            bool
		compressionSet                 bool
	}
	var given cli
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	has := func(names ...string) bool {
		for _, n := range names {
			if explicit[n] {
				return true
			}
		}
		return false
	}
	if has("root-dir", "d") {
		given.root, given.rootSet = opts.rootDir, true
	}
	if has("block-device", "b") {
		given.block, given.blockSet = opts.blockDevice, true
	}
	if has("bind", "i") {
		given.bind, given.bindSet = opts.bindIP, true
	}
	if has("port", "p") {
		given.port, given.portSet = opts.port, true
	}
	if has("sector-size", "s") {
		given.sector, given.sectorSet = opts.sectorSize, true
	}
	if has("read-only", "r") {
		given.readOnly, given.boolSet = opts.readOnly, true
	}
	if has("verbose", "v") {
		given.verbose, given.verboseSet = opts.verbose, true
	}
	if has("enable-compression", "c") {
		given.compression, given.compressionSet = opts.enableCompression, true
	}

	if configPath != "" {
		cfg, err := loadConfig(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
			os.Exit(1)
		}
		if cfg.RootDir != nil {
			opts.rootDir = *cfg.RootDir
		}
		if cfg.BlockDevice != nil {
			opts.blockDevice = *cfg.BlockDevice
		}
		if cfg.ReadOnly != nil {
			opts.readOnly = bool(*cfg.ReadOnly)
		}
		if cfg.EnableCompression != nil {
			opts.enableCompression = bool(*cfg.EnableCompression)
		}
		if cfg.Port != nil {
			opts.port = *cfg.Port
		}
		if cfg.Bind != nil {
			opts.bindIP = *cfg.Bind
		}
		if cfg.SectorSize != nil {
			opts.sectorSize = *cfg.SectorSize
		}
		if cfg.Verbose != nil {
			opts.verbose = bool(*cfg.Verbose)
		}
	}

	// Re-apply explicit CLI values on top of the config file.
	if given.rootSet {
		opts.rootDir = given.root
	}
	if given.blockSet {
		opts.blockDevice = given.block
	}
	if given.bindSet {
		opts.bindIP = given.bind
	}
	if given.portSet {
		opts.port = given.port
	}
	if given.sectorSet {
		opts.sectorSize = given.sector
	}
	if given.boolSet {
		opts.readOnly = given.readOnly
	}
	if given.verboseSet {
		opts.verbose = given.verbose
	}
	if given.compressionSet {
		opts.enableCompression = given.compression
	}

	if opts.rootDir == "" && opts.blockDevice == "" {
		fmt.Fprintln(os.Stderr, "Error: at least one of --root-dir or --block-device is required")
		os.Exit(1)
	}
	if opts.enableCompression {
		fmt.Fprintln(os.Stderr, "Warning: --enable-compression is not supported by the Go port; compressed files will be served as raw data")
	}
	if opts.sectorSize <= 0 {
		fmt.Fprintln(os.Stderr, "Error: --sector-size must be positive")
		os.Exit(1)
	}

	s, err := newServer(serverConfig{
		rootDir:     opts.rootDir,
		blockDevice: opts.blockDevice,
		port:        opts.port,
		bindIP:      opts.bindIP,
		sectorSize:  opts.sectorSize,
		readOnly:    opts.readOnly,
		verbose:     opts.verbose,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer s.close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s.run(ctx)
}
