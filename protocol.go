package main

// Wire protocol definitions for UDPFS over UDPRDMA (PS2 Neutrino).
// Layouts mirror udpfs_server.py exactly; all multi-byte fields are little endian.

import (
	"encoding/binary"
	"os"
	"time"
)

const (
	// UDPFS_PORT is the discovery port (0xF5F6).
	UDPFSPort = 0xF5F6
	// UDPRDMA service id advertised in DISCOVERY/INFORM.
	udpRDMASvcUDPFS = 0xF5F5
)

// UDPRDMA packet types (4 bits in the base header).
const (
	packetDiscovery uint16 = 0
	packetInform    uint16 = 1
	packetData      uint16 = 2
)

// UDPRDMA data flags (2 bits in the data header).
const (
	flagACK uint16 = 1
	flagFIN uint16 = 2
)

// UDPFS message types.
const (
	msgOpenReq      byte = 0x10
	msgOpenReply    byte = 0x11
	msgCloseReq     byte = 0x12
	msgCloseReply   byte = 0x13
	msgReadReq      byte = 0x14
	msgWriteReq     byte = 0x16
	msgWriteData    byte = 0x17
	msgWriteDone    byte = 0x18
	msgLSeekReq     byte = 0x1A
	msgLSeekReply   byte = 0x1B
	msgDReadReq     byte = 0x1C
	msgDReadReply   byte = 0x1D
	msgGetStatReq   byte = 0x1E
	msgGetStatReply byte = 0x1F
	msgMkdirReq     byte = 0x20
	msgRemoveReq    byte = 0x22
	msgRmdirReq     byte = 0x24
	msgResultReply  byte = 0x26
	msgBReadReq     byte = 0x28
	msgBWriteReq    byte = 0x2A
)

// PS2 iox stat mode bits.
const (
	fioSIFReg uint32 = 0x2000
	fioSIFDir uint32 = 0x1000
)

const (
	maxDataPayload    = 1408
	maxHandles        = 32
	sendWindow        = 8
	maxWindowRetries  = 4
	blockDeviceHandle = int32(0)
)

var windowAckTimeout = 100 * time.Millisecond

// POSIX errno values used on the wire (the PS2 side expects these numbers).
const (
	errnoEACCES  = 13
	errnoENOENT  = 2
	errnoEIO     = 5
	errnoEBADF   = 9
	errnoEEXIST  = 17
	errnoENOTDIR = 20
	errnoEISDIR  = 21
	errnoEINVAL  = 22
	errnoEMFILE  = 24
)

// Header: 2 bytes. packet_type:4, seq_nr:12.
type rdmaHeader struct {
	packetType uint16
	seqNr      uint16
}

func unpackHeader(b []byte) rdmaHeader {
	v := binary.LittleEndian.Uint16(b[:2])
	return rdmaHeader{packetType: v & 0xF, seqNr: (v >> 4) & 0xFFF}
}

func (h rdmaHeader) pack() []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, (h.packetType&0xF)|((h.seqNr&0xFFF)<<4))
	return b
}

// DiscHeader: 4 bytes. service_id, port (both little endian uint16).
type discHeader struct {
	serviceID uint16
	port      uint16
}

func unpackDiscHeader(b []byte) discHeader {
	return discHeader{
		serviceID: binary.LittleEndian.Uint16(b[:2]),
		port:      binary.LittleEndian.Uint16(b[2:4]),
	}
}

func (d discHeader) pack() []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b[:2], d.serviceID)
	binary.LittleEndian.PutUint16(b[2:], d.port)
	return b
}

// DataHeader: 4 bytes.
// seq_nr_ack:12, flags:2, hdr_word_count:4, data_byte_count:14.
type dataHeader struct {
	seqNrAck      uint16
	flags         uint16
	hdrWordCount  uint16
	dataByteCount uint16
}

func unpackDataHeader(b []byte) dataHeader {
	v := binary.LittleEndian.Uint32(b[:4])
	return dataHeader{
		seqNrAck:      uint16(v & 0xFFF),
		flags:         uint16((v >> 12) & 0x3),
		hdrWordCount:  uint16((v >> 14) & 0xF),
		dataByteCount: uint16((v >> 18) & 0x3FFF),
	}
}

func (d dataHeader) pack() []byte {
	v := uint32(d.seqNrAck&0xFFF) |
		uint32(d.flags&0x3)<<12 |
		uint32(d.hdrWordCount&0xF)<<14 |
		uint32(d.dataByteCount&0x3FFF)<<18
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

// statInfo holds PS2 iox_stat_t-compatible fields.
type statInfo struct {
	mode   uint32
	attr   uint32
	size   uint32
	hisize uint32
	ctime  [8]byte
	atime  [8]byte
	mtime  [8]byte
}

func zeroStat() statInfo {
	return statInfo{}
}

// encodeTime encodes a Unix timestamp into PS2 iox_stat_t time format (8 bytes):
// <BBBBBBH> = unused, sec, min, hour, mday, mon, year (year is tm_year = year-1900).
func encodeTime(t time.Time) [8]byte {
	var out [8]byte
	if t.IsZero() {
		return out
	}
	lt := t.In(time.Local)
	out[1] = byte(lt.Second())
	out[2] = byte(lt.Minute())
	out[3] = byte(lt.Hour())
	out[4] = byte(lt.Day())
	out[5] = byte(lt.Month())
	binary.LittleEndian.PutUint16(out[6:8], uint16(lt.Year()-1900))
	return out
}

func sizeToStat(size int64, mode uint32, fi os.FileInfo) statInfo {
	st := statInfo{
		mode:   mode,
		size:   uint32(size),
		hisize: uint32(uint64(size) >> 32),
	}
	if fi != nil {
		st.ctime = encodeTime(fi.ModTime())
		st.atime = st.ctime
		st.mtime = st.ctime
	}
	return st
}

func statFromFileInfo(fi os.FileInfo) statInfo {
	mode := uint32(0)
	if fi.IsDir() {
		mode = fioSIFDir
	} else if fi.Mode().IsRegular() {
		mode = fioSIFReg
	}
	return sizeToStat(fi.Size(), mode, fi)
}

// --- Response builders (little endian, byte-for-byte compatible with Python) ---

// OPEN_REPLY: <BBBBiIII> + ctime(8) + mtime(8) = 44 bytes.
func buildOpenReply(handle int32, st statInfo) []byte {
	b := make([]byte, 0, 44)
	b = append(b, msgOpenReply, 0, 0, 0)
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], uint32(handle))
	b = append(b, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], st.mode)
	b = append(b, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], st.size)
	b = append(b, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], st.hisize)
	b = append(b, tmp[:]...)
	b = append(b, st.ctime[:]...)
	b = append(b, st.mtime[:]...)
	return b
}

// CLOSE_REPLY / RESULT_REPLY / WRITE_DONE: <BBBBi> = 8 bytes.
func buildSimpleReply(msg byte, result int32) []byte {
	b := make([]byte, 8)
	b[0] = msg
	binary.LittleEndian.PutUint32(b[4:8], uint32(result))
	return b
}

// LSEEK_REPLY: <BBBBii> on error, <BBBBII> on success = 16 bytes.
func buildLSeekReply(position int64) []byte {
	b := make([]byte, 16)
	b[0] = msgLSeekReply
	if position < 0 {
		binary.LittleEndian.PutUint32(b[4:8], uint32(int32(position)))
		binary.LittleEndian.PutUint32(b[8:12], ^uint32(0))
	} else {
		binary.LittleEndian.PutUint32(b[4:8], uint32(position))
		binary.LittleEndian.PutUint32(b[8:12], uint32(uint64(position)>>32))
	}
	return b
}

// DREAD_REPLY: <BBHiIIII> + ctime/atime/mtime(24) + optional padded name.
func buildDReadReply(result int32, name string, st statInfo) []byte {
	nameBytes := append([]byte(name), 0)
	nameLen := uint16(len(nameBytes) - 1)
	padded := (len(nameBytes) + 3) &^ 3
	for len(nameBytes) < padded {
		nameBytes = append(nameBytes, 0)
	}

	b := make([]byte, 0, 48+padded+4)
	b = append(b, msgDReadReply, 0)
	var tmp [4]byte
	binary.LittleEndian.PutUint16(tmp[:2], nameLen)
	b = append(b, tmp[:2]...)
	binary.LittleEndian.PutUint32(tmp[:], uint32(result))
	b = append(b, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], st.mode)
	b = append(b, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], st.attr)
	b = append(b, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], st.size)
	b = append(b, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], st.hisize)
	b = append(b, tmp[:]...)
	b = append(b, st.ctime[:]...)
	b = append(b, st.atime[:]...)
	b = append(b, st.mtime[:]...)
	if result > 0 {
		b = append(b, nameBytes...)
	}
	return b
}

// GETSTAT_REPLY: <BBBBiIIII> + ctime/atime/mtime(24) = 52 bytes.
func buildGetStatReply(result int32, st statInfo) []byte {
	b := make([]byte, 0, 52)
	b = append(b, msgGetStatReply, 0, 0, 0)
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], uint32(result))
	b = append(b, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], st.mode)
	b = append(b, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], st.attr)
	b = append(b, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], st.size)
	b = append(b, tmp[:]...)
	binary.LittleEndian.PutUint32(tmp[:], st.hisize)
	b = append(b, tmp[:]...)
	b = append(b, st.ctime[:]...)
	b = append(b, st.atime[:]...)
	b = append(b, st.mtime[:]...)
	return b
}
