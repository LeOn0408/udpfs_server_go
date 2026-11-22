package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type txPacket struct {
	seq    uint16
	packet []byte
}

type fileHandle struct {
	f       *os.File // nil for directories
	isDir   bool
	dirPath string
	entries []os.DirEntry
	index   int
}

func (fh *fileHandle) close() {
	if fh.f != nil {
		_ = fh.f.Close()
		fh.f = nil
	}
}

type serverConfig struct {
	rootDir       string
	blockDevice   string
	port          int
	bindIP        string
	sectorSize    int
	readOnly      bool
	verbose       bool
	sectorSizeSet bool
}

type server struct {
	rootDir     string // absolute, symlink-resolved; empty if no filesystem root
	blockDevice string
	port        int
	bindIP      string
	sectorSize  int
	readOnly    bool
	verbose     bool

	discSock *net.UDPConn
	dataSock *net.UDPConn

	peerAddr *net.UDPAddr

	txSeqNr         uint16
	txSeqNrAcked    uint16
	rxSeqNrExpected uint16

	txBuffer   []txPacket
	txStartSeq uint16

	handles    map[int32]*fileHandle
	nextHandle int32

	bdSectorSize  int
	bdSectorCount int64

	// Shared write state (file and block writes).
	writeHandle         int32
	writeIsBlock        bool
	writeSectorNr       uint64
	writeSectorCount    uint16
	writeData           []byte
	writeTotalChunks    uint16
	writeReceivedChunks uint16

	stats map[string]int64

	startTime       time.Time
	lastStatusTime  time.Time
	lastStatusBytes int64
	statusVisible   bool
}

var statKeys = []string{
	"discovery", "open", "close", "read", "write", "bread", "bwrite",
	"lseek", "dread", "getstat", "mkdir", "remove", "rmdir",
	"bytes_read", "bytes_written",
}

func newServer(cfg serverConfig) (*server, error) {
	s := &server{
		port:         cfg.port,
		bindIP:       cfg.bindIP,
		sectorSize:   cfg.sectorSize,
		readOnly:     cfg.readOnly,
		verbose:      cfg.verbose,
		handles:      make(map[int32]*fileHandle),
		nextHandle:   1,
		bdSectorSize: cfg.sectorSize,
		stats:        make(map[string]int64),
		startTime:    time.Now(),
	}
	for _, k := range statKeys {
		s.stats[k] = 0
	}

	if cfg.rootDir != "" {
		abs, err := filepath.Abs(cfg.rootDir)
		if err != nil {
			return nil, fmt.Errorf("bad root dir: %w", err)
		}
		fi, err := os.Stat(abs)
		if err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("'%s' is not a directory", cfg.rootDir)
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			resolved = abs
		}
		s.rootDir = resolved
	}
	s.blockDevice = cfg.blockDevice

	if cfg.blockDevice != "" {
		if _, err := os.Stat(cfg.blockDevice); err != nil {
			return nil, fmt.Errorf("'%s' not found", cfg.blockDevice)
		}
		var f *os.File
		var err error
		if cfg.readOnly {
			f, err = os.Open(cfg.blockDevice)
		} else {
			f, err = os.OpenFile(cfg.blockDevice, os.O_RDWR, 0)
		}
		if err != nil {
			return nil, fmt.Errorf("cannot open '%s': %w", cfg.blockDevice, err)
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		s.bdSectorCount = st.Size() / int64(s.bdSectorSize)
		s.handles[blockDeviceHandle] = &fileHandle{f: f}
	}

	// Discovery socket: bound to the fixed UDPFS port, broadcast capable.
	lc := net.ListenConfig{Control: setSockopts}
	pc, err := lc.ListenPacket(context.Background(), "udp", fmt.Sprintf("0.0.0.0:%d", cfg.port))
	if err != nil {
		return nil, fmt.Errorf("bind discovery port %d: %w", cfg.port, err)
	}
	s.discSock = pc.(*net.UDPConn)

	// Data socket: ephemeral port, advertised in INFORM.
	pc2, err := lc.ListenPacket(context.Background(), "udp", fmt.Sprintf("%s:0", cfg.bindIP))
	if err != nil {
		_ = s.discSock.Close()
		return nil, fmt.Errorf("bind data socket: %w", err)
	}
	s.dataSock = pc2.(*net.UDPConn)

	return s, nil
}

func (s *server) close() {
	for _, fh := range s.handles {
		fh.close()
	}
	s.handles = make(map[int32]*fileHandle)
	if s.discSock != nil {
		_ = s.discSock.Close()
	}
	if s.dataSock != nil {
		_ = s.dataSock.Close()
	}
}

func (s *server) run(ctx context.Context) {
	fmt.Println("UDPFS Server")
	if s.rootDir != "" {
		fmt.Printf("  Root: %s\n", s.rootDir)
	}
	if _, ok := s.handles[blockDeviceHandle]; ok {
		fmt.Printf("  Block device: handle=%d, sectors=%d, sector_size=%d\n",
			blockDeviceHandle, s.bdSectorCount, s.bdSectorSize)
	}
	fmt.Printf("  Discovery bind: 0.0.0.0:%d (0x%04X)\n", s.port, s.port)
	_, dataPortStr, _ := net.SplitHostPort(s.dataSock.LocalAddr().String())
	dataPort, _ := strconv.Atoi(dataPortStr)
	fmt.Printf("  Data bind: %s:%d (0x%04X)\n", s.bindIP, dataPort, dataPort)
	if s.readOnly {
		fmt.Println("  Mode: read-only")
	} else {
		fmt.Println("  Mode: read-write")
	}
	fmt.Println("  Compression: disabled (not supported by Go port)")
	fmt.Println("  Listening...")
	fmt.Println()

	discBuf := make([]byte, 2048)
	dataBuf := make([]byte, 2048)
	for {
		select {
		case <-ctx.Done():
			if s.statusVisible {
				fmt.Printf("\r%-79s\r", "")
				s.statusVisible = false
			}
			fmt.Println("\nShutting down...")
			s.printStats()
			return
		default:
		}

		// Poll both sockets. Python uses select(); Go's UDPConn has no direct
		// select without raw FDs, so short read deadlines give the same effect.
		_ = s.discSock.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, addr, err := s.discSock.ReadFromUDP(discBuf)
		if err == nil && n >= 6 {
			s.handleDiscovery(discBuf[:n], addr)
		}

		_ = s.dataSock.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, addr, err = s.dataSock.ReadFromUDP(dataBuf)
		if err == nil && n >= 6 {
			s.handleData(dataBuf[:n], addr)
		}
	}
}

// --- Status / statistics output ---

func formatBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}

func (s *server) updateStatus() {
	now := time.Now()
	if now.Sub(s.lastStatusTime) < time.Second {
		return
	}
	elapsed := now.Sub(s.startTime)
	dt := now.Sub(s.lastStatusTime)
	if s.lastStatusTime.IsZero() {
		dt = elapsed
	}
	totalBytes := s.stats["bytes_read"] + s.stats["bytes_written"]
	bytesDelta := totalBytes - s.lastStatusBytes

	var ops []string
	for _, key := range []string{"bread", "read", "write", "bwrite", "open", "close", "dread", "getstat", "lseek"} {
		if v := s.stats[key]; v > 0 {
			ops = append(ops, fmt.Sprintf("%s:%d", key, v))
		}
	}
	opStr := strings.Join(ops, " ")
	if opStr == "" {
		opStr = "idle"
	}
	rate := float64(0)
	if dt > 0 {
		rate = float64(bytesDelta) / dt.Seconds()
	}
	h := int(elapsed / time.Hour)
	m := int((elapsed % time.Hour) / time.Minute)
	sec := int((elapsed % time.Minute) / time.Second)
	line := fmt.Sprintf("[%02d:%02d:%02d] %s | %s @ %s/s", h, m, sec, opStr,
		formatBytes(totalBytes), formatBytes(int64(rate)))
	fmt.Printf("\r%-79s", line)
	s.lastStatusTime = now
	s.lastStatusBytes = totalBytes
	s.statusVisible = true
}

func (s *server) printEvent(msg string) {
	if s.statusVisible {
		fmt.Printf("\r%-79s\r", "")
		s.statusVisible = false
	}
	fmt.Println(msg)
}

func (s *server) printStats() {
	elapsed := time.Since(s.startTime)
	h := int(elapsed / time.Hour)
	m := int((elapsed % time.Hour) / time.Minute)
	sec := int((elapsed % time.Minute) / time.Second)
	fmt.Println()
	fmt.Printf("Session: %02d:%02d:%02d\n", h, m, sec)
	fmt.Println()

	var ops []string
	for _, k := range statKeys {
		if k == "bytes_read" || k == "bytes_written" {
			continue
		}
		if v := s.stats[k]; v > 0 {
			ops = append(ops, fmt.Sprintf("  %-12s %10d", k, v))
		}
	}
	if len(ops) > 0 {
		fmt.Println("Operations:")
		for _, line := range ops {
			fmt.Println(line)
		}
	}
	read := s.stats["bytes_read"]
	written := s.stats["bytes_written"]
	if read > 0 || written > 0 {
		fmt.Println()
		fmt.Println("Transfer:")
		if read > 0 {
			rate := int64(0)
			if elapsed > 0 {
				rate = int64(float64(read) / elapsed.Seconds())
			}
			fmt.Printf("  read         %10s  (%s/s)\n", formatBytes(read), formatBytes(rate))
		}
		if written > 0 {
			rate := int64(0)
			if elapsed > 0 {
				rate = int64(float64(written) / elapsed.Seconds())
			}
			fmt.Printf("  written      %10s  (%s/s)\n", formatBytes(written), formatBytes(rate))
		}
	}
}

// --- OS error -> POSIX errno ---

func errnoFromError(err error) int32 {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch int(errno) {
		case 2, 3: // Windows FILE/PATH_NOT_FOUND
			return errnoENOENT
		case 5: // Windows ACCESS_DENIED
			return errnoEACCES
		case 183: // Windows ALREADY_EXISTS
			return errnoEEXIST
		case 267: // Windows DIRECTORY
			return errnoENOTDIR
		case errnoEACCES, errnoEBADF, errnoEEXIST, errnoENOTDIR,
			errnoEISDIR, errnoEINVAL, errnoEMFILE:
			return int32(errno)
		}
	}
	return errnoEIO
}

// --- Path resolution ---

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	missing := []string{filepath.Base(p)}
	cur := filepath.Dir(p)
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				r = filepath.Join(r, missing[i])
			}
			return filepath.Clean(r)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

// resolvePath maps a client path to an absolute path inside rootDir.
// An empty result means the path is rejected (traversal or no root dir).
func (s *server) resolvePath(clientPath string) string {
	if s.rootDir == "" {
		return ""
	}
	p := strings.TrimLeft(clientPath, "/\\")
	p = strings.ReplaceAll(p, "\\", "/")
	candidate := filepath.Clean(filepath.Join(s.rootDir, filepath.FromSlash(p)))
	resolved := realPath(candidate)
	rel, err := filepath.Rel(s.rootDir, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return ""
	}
	return resolved
}

// --- Handle management ---

func (s *server) allocHandle(fh *fileHandle) int32 {
	if len(s.handles) >= maxHandles {
		return -errnoEMFILE
	}
	id := s.nextHandle
	s.nextHandle++
	s.handles[id] = fh
	return id
}

func (s *server) freeHandle(id int32) {
	if id == blockDeviceHandle {
		return
	}
	if fh, ok := s.handles[id]; ok {
		fh.close()
		delete(s.handles, id)
	}
}
