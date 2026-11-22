package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// --- UDPRDMA packet handling ---

func (s *server) handleDiscovery(data []byte, addr *net.UDPAddr) {
	if len(data) < 6 {
		return
	}
	h := unpackHeader(data)
	if h.packetType != packetDiscovery {
		return
	}
	disc := unpackDiscHeader(data[2:6])
	if disc.serviceID != udpRDMASvcUDPFS {
		return
	}
	s.stats["discovery"]++
	s.peerAddr = addr
	s.printEvent(fmt.Sprintf("[%s] DISCOVERY -> INFORM", addr.String()))
	s.sendInform(addr)
}

func (s *server) handleData(data []byte, addr *net.UDPAddr) {
	if len(data) < 6 {
		return
	}
	h := unpackHeader(data)
	if h.packetType != packetData {
		return
	}
	dh := unpackDataHeader(data[2:6])
	payload := data[6:]
	payloadSize := int(dh.hdrWordCount)*4 + int(dh.dataByteCount)
	var actual []byte
	if payloadSize > 0 {
		if payloadSize > len(payload) {
			payloadSize = len(payload)
		}
		actual = payload[:payloadSize]
	}

	// Piggybacked ACK from any packet with the ACK flag.
	if dh.flags&flagACK != 0 {
		s.txSeqNrAcked = dh.seqNrAck
		s.trimTxBuffer(dh.seqNrAck)
	}

	// Pure ACK: nothing more to do.
	if payloadSize == 0 && dh.flags&flagACK != 0 {
		return
	}

	// NACK: seq_nr_ack is the expected (missing) sequence.
	if payloadSize == 0 && dh.flags&flagACK == 0 {
		s.txSeqNrAcked = (dh.seqNrAck - 1) & 0xFFF
		if s.verbose {
			s.printEvent(fmt.Sprintf("  NACK received, retransmit from seq=%d", dh.seqNrAck))
		}
		s.retransmitFrom(addr, dh.seqNrAck)
		return
	}

	if h.seqNr != s.rxSeqNrExpected {
		prev := (s.rxSeqNrExpected - 1) & 0xFFF
		switch {
		case h.seqNr == prev:
			// Duplicate of last processed packet: re-ACK and retransmit reply.
			if s.verbose {
				s.printEvent(fmt.Sprintf("  Duplicate seq=%d, re-ACK + retransmit", h.seqNr))
			}
			s.sendAck(addr, true)
			if len(s.txBuffer) > 0 {
				s.retransmitFrom(addr, s.txBuffer[0].seq)
			}
			return
		case h.seqNr == 0:
			// Peer reestablished: reset connection state.
			if s.verbose {
				s.printEvent("  Received seq=0, assuming the peer was reset")
			}
			s.txSeqNr = 0
			s.txBuffer = nil
			s.txStartSeq = 0
			s.txSeqNrAcked = 0
			s.rxSeqNrExpected = 0
		default:
			if s.verbose {
				s.printEvent(fmt.Sprintf("  WARNING: Expected seq=%d, got %d", s.rxSeqNrExpected, h.seqNr))
			}
			s.sendAck(addr, false)
			return
		}
	}
	s.rxSeqNrExpected = (s.rxSeqNrExpected + 1) & 0xFFF

	// Immediate ACK so the PS2's udprdma_send() can return quickly.
	s.sendAck(addr, true)

	if len(actual) == 0 {
		return
	}

	switch actual[0] {
	case msgOpenReq:
		s.handleOpen(addr, actual)
	case msgCloseReq:
		s.handleClose(addr, actual)
	case msgReadReq:
		s.handleRead(addr, actual)
	case msgWriteReq:
		s.handleWriteReq(addr, actual)
	case msgWriteData:
		s.handleWriteData(addr, actual)
	case msgLSeekReq:
		s.handleLSeek(addr, actual)
	case msgDReadReq:
		s.handleDRead(addr, actual)
	case msgGetStatReq:
		s.handleGetStat(addr, actual)
	case msgMkdirReq:
		s.handleMkdir(addr, actual)
	case msgRemoveReq:
		s.handleRemove(addr, actual)
	case msgRmdirReq:
		s.handleRmDir(addr, actual)
	case msgBReadReq:
		s.handleBRead(addr, actual)
	case msgBWriteReq:
		s.handleBWriteReq(addr, actual)
	default:
		s.printEvent(fmt.Sprintf("[%s] Unknown message type: 0x%02x", addr.String(), actual[0]))
		s.sendAck(addr, true)
	}
}

// --- Send helpers ---

func (s *server) sendPacket(addr *net.UDPAddr, packet []byte) {
	if _, err := s.dataSock.WriteToUDP(packet, addr); err != nil && s.verbose {
		s.printEvent(fmt.Sprintf("  sendto %s: %v", addr.String(), err))
	}
}

func (s *server) sendInform(addr *net.UDPAddr) {
	local := s.dataSock.LocalAddr().(*net.UDPAddr)
	packet := append(rdmaHeader{packetType: packetInform, seqNr: 1}.pack(),
		discHeader{serviceID: udpRDMASvcUDPFS, port: uint16(local.Port)}.pack()...)
	s.sendPacket(addr, packet)
}

func (s *server) sendAck(addr *net.UDPAddr, isAck bool) {
	seqAck := s.rxSeqNrExpected
	flags := uint16(0)
	if isAck {
		seqAck = (s.rxSeqNrExpected - 1) & 0xFFF
		flags = flagACK
	}
	packet := append(rdmaHeader{packetType: packetData, seqNr: s.txSeqNr}.pack(),
		dataHeader{seqNrAck: seqAck, flags: flags}.pack()...)
	s.sendPacket(addr, packet)
}

func (s *server) sendData(addr *net.UDPAddr, payload []byte) {
	padded := (len(payload) + 3) &^ 3
	buf := make([]byte, len(payload), padded)
	copy(buf, payload)
	for len(buf) < padded {
		buf = append(buf, 0)
	}
	packet := append(rdmaHeader{packetType: packetData, seqNr: s.txSeqNr}.pack(),
		dataHeader{
			seqNrAck:      (s.rxSeqNrExpected - 1) & 0xFFF,
			flags:         flagACK | flagFIN,
			dataByteCount: uint16(padded),
		}.pack()...)
	packet = append(packet, buf...)
	s.sendPacket(addr, packet)
	s.txSeqNr = (s.txSeqNr + 1) & 0xFFF
}

// sendDataPacket stores the packet for retransmission (unlike sendData).
func (s *server) sendDataPacket(addr *net.UDPAddr, payload []byte, fin bool, hdrSize int) {
	dataSize := len(payload) - hdrSize
	paddedDataSize := (dataSize + 3) &^ 3
	buf := make([]byte, 0, hdrSize+paddedDataSize)
	buf = append(buf, payload[:hdrSize]...)
	buf = append(buf, payload[hdrSize:]...)
	for len(buf) < hdrSize+paddedDataSize {
		buf = append(buf, 0)
	}
	flags := flagACK
	if fin {
		flags |= flagFIN
	}
	packet := append(rdmaHeader{packetType: packetData, seqNr: s.txSeqNr}.pack(),
		dataHeader{
			seqNrAck:      (s.rxSeqNrExpected - 1) & 0xFFF,
			flags:         flags,
			hdrWordCount:  uint16(hdrSize / 4),
			dataByteCount: uint16(paddedDataSize),
		}.pack()...)
	packet = append(packet, buf...)

	s.txBuffer = append(s.txBuffer, txPacket{seq: s.txSeqNr, packet: packet})
	s.sendPacket(addr, packet)
	s.txSeqNr = (s.txSeqNr + 1) & 0xFFF
}

func (s *server) trimTxBuffer(ack uint16) {
	kept := s.txBuffer[:0]
	for _, tp := range s.txBuffer {
		if ((tp.seq - ack - 1) & 0xFFF) < 2048 {
			kept = append(kept, tp)
		}
	}
	s.txBuffer = kept
}

func (s *server) retransmitFrom(addr *net.UDPAddr, fromSeq uint16) {
	count := 0
	for _, tp := range s.txBuffer {
		if ((tp.seq - fromSeq) & 0xFFF) < 2048 {
			s.sendPacket(addr, tp.packet)
			count++
		}
	}
	if s.verbose && count > 0 {
		s.printEvent(fmt.Sprintf("  Retransmitted %d packets from seq=%d", count, fromSeq))
	}
}

func optimalChunkSize(total int) int {
	ceilDiv := func(n, d int) int { return (n + d - 1) / d }
	type candidate struct{ max, align int }
	candidates := []candidate{{1024, 512}, {1280, 256}, {1408, 128}}
	bestChunk := 1408
	bestPackets := ceilDiv(total, 1408)
	bestAlign := 128
	for _, c := range candidates {
		packets := ceilDiv(total, c.max)
		if packets < bestPackets || (packets == bestPackets && c.align > bestAlign) {
			bestPackets = packets
			bestChunk = c.max
			bestAlign = c.align
		}
	}
	return bestChunk
}

func (s *server) waitForWindowAck(addr *net.UDPAddr) {
	_ = s.dataSock.SetReadDeadline(time.Now().Add(windowAckTimeout))
	defer func() { _ = s.dataSock.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, 2048)
	for {
		n, _, err := s.dataSock.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() && len(s.txBuffer) > 0 {
				s.retransmitFrom(addr, s.txBuffer[0].seq)
			}
			return
		}
		if n < 6 {
			continue
		}
		h := unpackHeader(buf[:n])
		if h.packetType != packetData {
			continue
		}
		dh := unpackDataHeader(buf[2:6])
		if dh.dataByteCount > 0 || dh.hdrWordCount > 0 {
			continue // not a pure ACK/NACK
		}
		if dh.flags&flagACK != 0 {
			s.txSeqNrAcked = dh.seqNrAck
			s.trimTxBuffer(dh.seqNrAck)
			return
		}
		s.txSeqNrAcked = (dh.seqNrAck - 1) & 0xFFF
		s.retransmitFrom(addr, dh.seqNrAck)
		return
	}
}

func (s *server) inFlight() uint16 {
	return (s.txSeqNr - s.txSeqNrAcked - 1) & 0xFFF
}

func (s *server) waitForAck(addr *net.UDPAddr, timeout time.Duration) bool {
	_ = s.dataSock.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = s.dataSock.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, 2048)
	for {
		n, _, err := s.dataSock.ReadFromUDP(buf)
		if err != nil {
			return false
		}
		if n < 6 {
			continue
		}
		h := unpackHeader(buf[:n])
		if h.packetType != packetData {
			continue
		}
		dh := unpackDataHeader(buf[2:6])
		if dh.dataByteCount == 0 && dh.hdrWordCount == 0 {
			if dh.flags&flagACK != 0 {
				s.txBuffer = nil
				return true
			}
			s.retransmitFrom(addr, dh.seqNrAck)
		}
	}
}

func (s *server) sendRawData(addr *net.UDPAddr, data []byte) {
	s.txBuffer = nil
	s.txStartSeq = s.txSeqNr
	chunk := optimalChunkSize(len(data))

	offset := 0
	windowRetries := 0
	for offset < len(data) {
		if s.inFlight() >= sendWindow {
			oldAcked := s.txSeqNrAcked
			s.waitForWindowAck(addr)
			if s.txSeqNrAcked == oldAcked {
				windowRetries++
				if windowRetries >= maxWindowRetries {
					s.printEvent("  Window ACK retries exhausted, aborting transfer")
					return
				}
			} else {
				windowRetries = 0
			}
			continue
		}
		windowRetries = 0
		size := chunk
		if size > len(data)-offset {
			size = len(data) - offset
		}
		last := offset+size >= len(data)
		s.sendDataPacket(addr, data[offset:offset+size], last, 0)
		offset += size
	}
}

func (s *server) sendRawDataWithHeader(addr *net.UDPAddr, header, data []byte) {
	s.txBuffer = nil
	s.txStartSeq = s.txSeqNr
	chunk := optimalChunkSize(len(data))

	firstMax := chunk
	if maxDataPayload-len(header) < firstMax {
		firstMax = maxDataPayload - len(header)
	}
	firstSize := firstMax
	if firstSize > len(data) {
		firstSize = len(data)
	}
	last := firstSize >= len(data)
	first := make([]byte, 0, len(header)+firstSize)
	first = append(first, header...)
	first = append(first, data[:firstSize]...)
	s.sendDataPacket(addr, first, last, len(header))

	offset := firstSize
	windowRetries := 0
	for offset < len(data) {
		if s.inFlight() >= sendWindow {
			oldAcked := s.txSeqNrAcked
			s.waitForWindowAck(addr)
			if s.txSeqNrAcked == oldAcked {
				windowRetries++
				if windowRetries >= maxWindowRetries {
					s.printEvent("  Window ACK retries exhausted, aborting transfer")
					return
				}
			} else {
				windowRetries = 0
			}
			continue
		}
		windowRetries = 0
		size := chunk
		if size > len(data)-offset {
			size = len(data) - offset
		}
		last = offset+size >= len(data)
		s.sendDataPacket(addr, data[offset:offset+size], last, 0)
		offset += size
	}
}

// --- Response senders ---

func (s *server) sendOpenReply(addr *net.UDPAddr, handle int32, st *statInfo) {
	if st == nil {
		zero := zeroStat()
		st = &zero
	}
	s.sendData(addr, buildOpenReply(handle, *st))
}

func (s *server) sendCloseReply(addr *net.UDPAddr, result int32) {
	s.sendData(addr, buildSimpleReply(msgCloseReply, result))
}

func (s *server) sendResultReply(addr *net.UDPAddr, result int32) {
	s.sendData(addr, buildSimpleReply(msgResultReply, result))
}

func (s *server) sendWriteDone(addr *net.UDPAddr, result int32) {
	s.sendData(addr, buildSimpleReply(msgWriteDone, result))
}

func (s *server) sendLSeekReply(addr *net.UDPAddr, position int64) {
	s.sendData(addr, buildLSeekReply(position))
}

func (s *server) sendReadResult(addr *net.UDPAddr, result int32, data []byte) {
	s.sendRawDataWithHeader(addr, buildSimpleReply(msgResultReply, result), data)
}

func (s *server) sendDReadReply(addr *net.UDPAddr, result int32, name string, st *statInfo) {
	if st == nil {
		zero := zeroStat()
		st = &zero
	}
	s.sendData(addr, buildDReadReply(result, name, *st))
}

func (s *server) sendGetStatReply(addr *net.UDPAddr, result int32, st *statInfo) {
	if st == nil {
		zero := zeroStat()
		st = &zero
	}
	s.sendData(addr, buildGetStatReply(result, *st))
}

// --- Small payload helpers ---

func cString(data []byte) string {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		data = data[:i]
	}
	return string(data)
}

func u16le(b []byte, off int) uint16 { return binary.LittleEndian.Uint16(b[off : off+2]) }
func u32le(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off : off+4]) }
func i32le(b []byte, off int) int32  { return int32(u32le(b, off)) }

// --- Filesystem helpers ---

// openFlags maps PS2 open flags to Go os.OpenFile flags.
// PS2: O_RDONLY=1, O_WRONLY=2, O_RDWR=3, O_APPEND=0x100, O_CREAT=0x200, O_TRUNC=0x400.
func openFlags(flags uint16, fileExists bool) int {
	access := flags & 0x3
	switch access {
	case 1: // O_RDONLY -> 'rb'
		return os.O_RDONLY
	case 2: // O_WRONLY
		if flags&0x100 != 0 { // 'ab'
			return os.O_WRONLY | os.O_CREATE | os.O_APPEND
		}
		if flags&0x400 != 0 { // 'wb'
			return os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		if flags&0x200 != 0 {
			if fileExists { // 'r+b'
				return os.O_RDWR
			}
			return os.O_WRONLY | os.O_CREATE | os.O_TRUNC // 'wb'
		}
		return os.O_RDWR // 'r+b'
	case 3: // O_RDWR
		if flags&0x200 != 0 {
			if flags&0x400 != 0 { // 'w+b'
				return os.O_RDWR | os.O_CREATE | os.O_TRUNC
			}
			if flags&0x100 != 0 { // 'a+b'
				return os.O_RDWR | os.O_CREATE | os.O_APPEND
			}
			if fileExists { // 'r+b'
				return os.O_RDWR
			}
			return os.O_RDWR | os.O_CREATE | os.O_TRUNC // 'w+b'
		}
		return os.O_RDWR // 'r+b'
	}
	return os.O_RDONLY
}

func readUpTo(f *os.File, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return buf[:got], nil
	}
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// --- Message handlers ---

func (s *server) handleOpen(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 8 {
		s.sendOpenReply(addr, -errnoEINVAL, nil)
		return
	}
	isDir := payload[1] != 0
	flags := u16le(payload, 2)
	path := cString(payload[8:])
	s.stats["open"]++

	resolved := s.resolvePath(path)
	if resolved == "" {
		s.printEvent(fmt.Sprintf("[%s] OPEN '%s' -> EACCES (path traversal or no root_dir)", addr.String(), path))
		s.sendOpenReply(addr, -errnoEACCES, nil)
		return
	}

	if isDir {
		f, err := os.Open(resolved)
		if err != nil {
			s.printEvent(fmt.Sprintf("[%s] DOPEN '%s' -> error: %v", addr.String(), path, err))
			s.sendOpenReply(addr, errnoFromError(err), nil)
			return
		}
		entries, err := f.ReadDir(-1)
		_ = f.Close()
		if err != nil {
			s.printEvent(fmt.Sprintf("[%s] DOPEN '%s' -> error: %v", addr.String(), path, err))
			s.sendOpenReply(addr, errnoFromError(err), nil)
			return
		}
		handle := s.allocHandle(&fileHandle{isDir: true, dirPath: resolved, entries: entries})
		if handle < 0 {
			s.sendOpenReply(addr, handle, nil)
			return
		}
		st, err := os.Stat(resolved)
		if err != nil {
			s.freeHandle(handle)
			s.sendOpenReply(addr, errnoFromError(err), nil)
			return
		}
		info := statFromFileInfo(st)
		s.printEvent(fmt.Sprintf("[%s] DOPEN '%s' -> handle=%d", addr.String(), path, handle))
		s.sendOpenReply(addr, handle, &info)
		return
	}

	if s.readOnly && flags&0x02 != 0 {
		s.printEvent(fmt.Sprintf("[%s] OPEN '%s' -> EACCES (read-only)", addr.String(), path))
		s.sendOpenReply(addr, -errnoEACCES, nil)
		return
	}

	_, statErr := os.Stat(resolved)
	fileExists := statErr == nil
	if !fileExists && flags&0x200 == 0 {
		s.printEvent(fmt.Sprintf("[%s] OPEN '%s' -> ENOENT (file not found)", addr.String(), path))
		s.sendOpenReply(addr, -errnoENOENT, nil)
		return
	}

	goFlags := openFlags(flags, fileExists)
	if flags&0x200 != 0 {
		parent := filepath.Dir(resolved)
		if _, err := os.Stat(parent); err != nil {
			_ = os.MkdirAll(parent, 0755)
		}
	}
	f, err := os.OpenFile(resolved, goFlags, 0666)
	if err != nil {
		s.printEvent(fmt.Sprintf("[%s] OPEN '%s' -> error: %v", addr.String(), path, err))
		s.sendOpenReply(addr, errnoFromError(err), nil)
		return
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		s.sendOpenReply(addr, errnoFromError(err), nil)
		return
	}
	info := statFromFileInfo(fi)
	handle := s.allocHandle(&fileHandle{f: f})
	if handle < 0 {
		_ = f.Close()
		s.sendOpenReply(addr, handle, nil)
		return
	}
	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] OPEN '%s' -> handle=%d, size=%d", addr.String(), path, handle, fi.Size()))
	}
	s.sendOpenReply(addr, handle, &info)
}

func (s *server) handleClose(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 8 {
		s.sendCloseReply(addr, -errnoEINVAL)
		return
	}
	handle := i32le(payload, 4)
	s.stats["close"]++
	if handle == blockDeviceHandle {
		s.sendCloseReply(addr, 0)
		return
	}
	if _, ok := s.handles[handle]; !ok {
		s.sendCloseReply(addr, -errnoEBADF)
		return
	}
	s.freeHandle(handle)
	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] CLOSE handle=%d", addr.String(), handle))
	}
	s.sendCloseReply(addr, 0)
}

func (s *server) handleRead(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 12 {
		s.sendReadResult(addr, -errnoEINVAL, nil)
		return
	}
	handle := i32le(payload, 4)
	size := int(u32le(payload, 8))
	s.stats["read"]++

	fh, ok := s.handles[handle]
	if !ok || fh.isDir {
		s.sendReadResult(addr, -errnoEBADF, nil)
		return
	}
	data, err := readUpTo(fh.f, size)
	if err != nil {
		s.printEvent(fmt.Sprintf("[%s] READ error: %v", addr.String(), err))
		s.sendReadResult(addr, errnoFromError(err), nil)
		return
	}
	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] READ handle=%d size=%d -> %d bytes", addr.String(), handle, size, len(data)))
	}
	s.stats["bytes_read"] += int64(len(data))
	s.updateStatus()
	s.sendReadResult(addr, int32(len(data)), data)
}

func (s *server) handleWriteReq(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 12 {
		s.sendAck(addr, true)
		return
	}
	handle := i32le(payload, 4)
	s.stats["write"]++

	if s.readOnly {
		s.printEvent(fmt.Sprintf("[%s] WRITE -> EACCES (read-only)", addr.String()))
		s.sendWriteDone(addr, -errnoEACCES)
		return
	}
	fh, ok := s.handles[handle]
	if !ok || fh.isDir {
		s.sendWriteDone(addr, -errnoEBADF)
		return
	}
	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] WRITE_REQ handle=%d size=%d", addr.String(), handle, u32le(payload, 8)))
	}
	s.writeHandle = handle
	s.writeIsBlock = false
	s.writeData = nil
	s.writeTotalChunks = 0
	s.writeReceivedChunks = 0

	if len(payload) > 12 {
		s.handleWriteData(addr, payload[12:])
	} else {
		s.sendAck(addr, true)
	}
}

func (s *server) handleWriteData(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 8 {
		s.sendAck(addr, true)
		return
	}
	chunkNr := payload[2]
	chunkSize := int(u16le(payload, 4))
	totalChunks := u16le(payload, 6)
	end := 8 + chunkSize
	if end > len(payload) {
		end = len(payload)
	}
	chunkData := payload[8:end]

	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] WRITE_DATA chunk=%d/%d size=%d", addr.String(), chunkNr, totalChunks, chunkSize))
	}
	if int(chunkNr) != int(s.writeReceivedChunks) {
		s.printEvent(fmt.Sprintf("  ERROR: Chunk order error (expected %d, got %d)", s.writeReceivedChunks, chunkNr))
		s.sendAck(addr, true)
		return
	}
	s.writeData = append(s.writeData, chunkData...)
	s.writeTotalChunks = totalChunks
	s.writeReceivedChunks++

	if s.writeReceivedChunks >= s.writeTotalChunks {
		if s.writeIsBlock {
			s.completeBWrite(addr)
		} else {
			s.completeWrite(addr)
		}
	} else {
		s.sendAck(addr, true)
	}
}

func (s *server) completeWrite(addr *net.UDPAddr) {
	fh, ok := s.handles[s.writeHandle]
	if !ok || fh.isDir {
		s.sendWriteDone(addr, -errnoEBADF)
		return
	}
	n, err := fh.f.Write(s.writeData)
	if err == nil {
		err = fh.f.Sync()
	}
	if err != nil {
		s.printEvent(fmt.Sprintf("[%s] WRITE error: %v", addr.String(), err))
		s.sendWriteDone(addr, errnoFromError(err))
		return
	}
	s.stats["bytes_written"] += int64(n)
	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] WRITE handle=%d -> %d bytes", addr.String(), s.writeHandle, n))
	}
	s.updateStatus()
	s.sendWriteDone(addr, int32(n))
}

func (s *server) completeBWrite(addr *net.UDPAddr) {
	fh, ok := s.handles[s.writeHandle]
	if !ok || fh.isDir {
		s.sendWriteDone(addr, -errnoEBADF)
		return
	}
	sectorSize := s.bdSectorSize
	if s.writeHandle != blockDeviceHandle {
		sectorSize = 512
	}
	expectedSize := int(s.writeSectorCount) * sectorSize
	data := s.writeData
	if len(data) > expectedSize {
		data = data[:expectedSize]
	}
	_, err := fh.f.Seek(int64(s.writeSectorNr)*int64(sectorSize), io.SeekStart)
	if err == nil {
		_, err = fh.f.Write(data)
	}
	if err == nil {
		err = fh.f.Sync()
	}
	if err != nil {
		s.printEvent(fmt.Sprintf("[%s] BWRITE error: %v", addr.String(), err))
		s.sendWriteDone(addr, errnoFromError(err))
		return
	}
	s.stats["bytes_written"] += int64(expectedSize)
	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] BWRITE handle=%d sector=%d count=%d",
			addr.String(), s.writeHandle, s.writeSectorNr, s.writeSectorCount))
	}
	s.updateStatus()
	s.sendWriteDone(addr, 0)
}

func (s *server) handleLSeek(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 16 {
		s.sendLSeekReply(addr, -errnoEINVAL)
		return
	}
	whence := payload[1]
	handle := i32le(payload, 4)
	lo := u32le(payload, 8)
	hi := u32le(payload, 12)
	offset := int64(uint64(hi)<<32 | uint64(lo))
	s.stats["lseek"]++

	fh, ok := s.handles[handle]
	if !ok || fh.isDir {
		s.sendLSeekReply(addr, -errnoEBADF)
		return
	}
	pos, err := fh.f.Seek(offset, int(whence))
	if err != nil {
		s.printEvent(fmt.Sprintf("[%s] LSEEK error: %v", addr.String(), err))
		s.sendLSeekReply(addr, int64(errnoFromError(err)))
		return
	}
	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] LSEEK handle=%d offset=%d whence=%d -> %d",
			addr.String(), handle, offset, whence, pos))
	}
	s.sendLSeekReply(addr, pos)
}

func (s *server) handleDRead(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 8 {
		s.sendDReadReply(addr, -errnoEINVAL, "", nil)
		return
	}
	handle := i32le(payload, 4)
	s.stats["dread"]++

	fh, ok := s.handles[handle]
	if !ok || !fh.isDir {
		s.sendDReadReply(addr, -errnoEBADF, "", nil)
		return
	}
	if fh.index >= len(fh.entries) {
		if s.verbose {
			s.printEvent(fmt.Sprintf("[%s] DREAD handle=%d -> end of dir", addr.String(), handle))
		}
		s.sendDReadReply(addr, 0, "", nil)
		return
	}
	entry := fh.entries[fh.index]
	fh.index++
	st, err := os.Lstat(filepath.Join(fh.dirPath, entry.Name()))
	if err != nil {
		s.printEvent(fmt.Sprintf("[%s] DREAD stat error: %v", addr.String(), err))
		s.sendDReadReply(addr, 0, "", nil)
		return
	}
	info := statFromFileInfo(st)
	name := entry.Name()
	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] DREAD handle=%d -> '%s'", addr.String(), handle, name))
	}
	s.sendDReadReply(addr, 1, name, &info)
}

func (s *server) handleGetStat(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 4 {
		s.sendGetStatReply(addr, -errnoEINVAL, nil)
		return
	}
	path := cString(payload[4:])
	s.stats["getstat"]++

	if path == "" {
		if _, ok := s.handles[blockDeviceHandle]; ok {
			total := int64(s.bdSectorSize) * s.bdSectorCount
			info := sizeToStat(total, 0, nil)
			if s.verbose {
				s.printEvent(fmt.Sprintf("[%s] GETSTAT '' -> block device %d bytes", addr.String(), total))
			}
			s.sendGetStatReply(addr, 0, &info)
			return
		}
	}

	resolved := s.resolvePath(path)
	if resolved == "" {
		s.sendGetStatReply(addr, -errnoEACCES, nil)
		return
	}
	st, err := os.Stat(resolved)
	if err != nil {
		if s.verbose {
			s.printEvent(fmt.Sprintf("[%s] GETSTAT '%s' -> error: %v", addr.String(), path, err))
		}
		s.sendGetStatReply(addr, errnoFromError(err), nil)
		return
	}
	info := statFromFileInfo(st)
	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] GETSTAT '%s' -> size=%d", addr.String(), path, info.size))
	}
	s.sendGetStatReply(addr, 0, &info)
}

func (s *server) handleMkdir(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 4 {
		s.sendResultReply(addr, -errnoEINVAL)
		return
	}
	mode := u16le(payload, 2)
	path := cString(payload[4:])
	s.stats["mkdir"]++

	if s.readOnly {
		s.sendResultReply(addr, -errnoEACCES)
		return
	}
	resolved := s.resolvePath(path)
	if resolved == "" {
		s.sendResultReply(addr, -errnoEACCES)
		return
	}
	perm := os.FileMode(0755)
	if mode != 0 {
		perm = os.FileMode(mode & 0777)
	}
	if err := os.Mkdir(resolved, perm); err != nil {
		s.printEvent(fmt.Sprintf("[%s] MKDIR '%s' -> error: %v", addr.String(), path, err))
		s.sendResultReply(addr, errnoFromError(err))
		return
	}
	s.printEvent(fmt.Sprintf("[%s] MKDIR '%s'", addr.String(), path))
	s.sendResultReply(addr, 0)
}

func (s *server) handleRemove(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 4 {
		s.sendResultReply(addr, -errnoEINVAL)
		return
	}
	path := cString(payload[4:])
	s.stats["remove"]++
	if s.readOnly {
		s.sendResultReply(addr, -errnoEACCES)
		return
	}
	resolved := s.resolvePath(path)
	if resolved == "" {
		s.sendResultReply(addr, -errnoEACCES)
		return
	}
	if err := os.Remove(resolved); err != nil {
		s.printEvent(fmt.Sprintf("[%s] REMOVE '%s' -> error: %v", addr.String(), path, err))
		s.sendResultReply(addr, errnoFromError(err))
		return
	}
	s.printEvent(fmt.Sprintf("[%s] REMOVE '%s'", addr.String(), path))
	s.sendResultReply(addr, 0)
}

func (s *server) handleRmDir(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 4 {
		s.sendResultReply(addr, -errnoEINVAL)
		return
	}
	path := cString(payload[4:])
	s.stats["rmdir"]++
	if s.readOnly {
		s.sendResultReply(addr, -errnoEACCES)
		return
	}
	resolved := s.resolvePath(path)
	if resolved == "" {
		s.sendResultReply(addr, -errnoEACCES)
		return
	}
	if err := os.Remove(resolved); err != nil {
		s.printEvent(fmt.Sprintf("[%s] RMDIR '%s' -> error: %v", addr.String(), path, err))
		s.sendResultReply(addr, errnoFromError(err))
		return
	}
	s.printEvent(fmt.Sprintf("[%s] RMDIR '%s'", addr.String(), path))
	s.sendResultReply(addr, 0)
}

func (s *server) handleBRead(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 16 {
		s.sendReadResult(addr, -errnoEINVAL, nil)
		return
	}
	sectorCount := int(u16le(payload, 2))
	handle := i32le(payload, 4)
	sectorNr := uint64(u32le(payload, 8)) | uint64(u32le(payload, 12))<<32
	s.stats["bread"]++

	fh, ok := s.handles[handle]
	if !ok || fh.isDir {
		s.sendReadResult(addr, -errnoEBADF, nil)
		return
	}
	sectorSize := s.bdSectorSize
	if handle != blockDeviceHandle {
		sectorSize = 512
	}
	totalSize := sectorCount * sectorSize
	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] BREAD handle=%d sector=%d count=%d (%d bytes)",
			addr.String(), handle, sectorNr, sectorCount, totalSize))
	}
	if _, err := fh.f.Seek(int64(sectorNr)*int64(sectorSize), io.SeekStart); err != nil {
		s.printEvent(fmt.Sprintf("[%s] BREAD error: %v", addr.String(), err))
		s.sendReadResult(addr, errnoFromError(err), nil)
		return
	}
	data, err := readUpTo(fh.f, totalSize)
	if err != nil {
		s.printEvent(fmt.Sprintf("[%s] BREAD error: %v", addr.String(), err))
		s.sendReadResult(addr, errnoFromError(err), nil)
		return
	}
	s.stats["bytes_read"] += int64(len(data))
	s.updateStatus()
	s.sendReadResult(addr, int32(len(data)), data)
}

func (s *server) handleBWriteReq(addr *net.UDPAddr, payload []byte) {
	if len(payload) < 16 {
		s.sendAck(addr, true)
		return
	}
	sectorCount := u16le(payload, 2)
	handle := i32le(payload, 4)
	sectorNr := uint64(u32le(payload, 8)) | uint64(u32le(payload, 12))<<32
	s.stats["bwrite"]++

	if s.readOnly {
		s.printEvent(fmt.Sprintf("[%s] BWRITE -> EACCES (read-only)", addr.String()))
		s.sendWriteDone(addr, -errnoEACCES)
		return
	}
	if _, ok := s.handles[handle]; !ok || s.handles[handle].isDir {
		s.sendWriteDone(addr, -errnoEBADF)
		return
	}
	if s.verbose {
		s.printEvent(fmt.Sprintf("[%s] BWRITE_REQ handle=%d sector=%d count=%d",
			addr.String(), handle, sectorNr, sectorCount))
	}
	s.writeHandle = handle
	s.writeIsBlock = true
	s.writeSectorNr = sectorNr
	s.writeSectorCount = sectorCount
	s.writeData = nil
	s.writeTotalChunks = 0
	s.writeReceivedChunks = 0

	if len(payload) > 16 {
		s.handleWriteData(addr, payload[16:])
	} else {
		s.sendAck(addr, true)
	}
}
