package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/at-wat/ebml-go/webm"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v2"
	"github.com/pion/webrtc/v2/pkg/media/samplebuilder"
)

type diskClient struct {
	group *group
	id    string

	mu     sync.Mutex
	down   []*diskConn
	closed bool
}

func (client *diskClient) getGroup() *group {
	return client.group
}

func (client *diskClient) getId() string {
	return client.id
}

func (client *diskClient) getUsername() string {
	return "RECORDING"
}

func (client *diskClient) pushClient(id, username string, add bool) error {
	return nil
}

func (client *diskClient) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()

	for _, down := range client.down {
		down.Close()
	}
	client.down = nil
	client.closed = true
	return nil
}

func (client *diskClient) pushConn(conn *upConnection, tracks []*upTrack, label string) error {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.closed {
		return errors.New("disk client is closed")
	}

	directory := filepath.Join(recordingsDir, client.group.name)
	err := os.MkdirAll(directory, 0700)
	if err != nil {
		return err
	}

	down, err := newDiskConn(directory, label, conn, tracks)
	if err != nil {
		return err
	}

	client.down = append(client.down, down)
	return nil
}

var _ client = &diskClient{}

type diskConn struct {
	directory string
	label     string

	mu            sync.Mutex
	file          *os.File
	remote        *upConnection
	tracks        []*diskTrack
	width, height uint32
}

// called locked
func (conn *diskConn) reopen() error {
	for _, t := range conn.tracks {
		if t.writer != nil {
			t.writer.Close()
			t.writer = nil
		}
	}
	conn.file = nil

	file, err := openDiskFile(conn.directory, conn.label)
	if err != nil {
		return err
	}

	conn.file = file
	return nil
}

func (conn *diskConn) Close() error {
	conn.remote.delLocal(conn)

	conn.mu.Lock()
	tracks := make([]*diskTrack, 0, len(conn.tracks))
	for _, t := range conn.tracks {
		if t.writer != nil {
			t.writer.Close()
			t.writer = nil
		}
		tracks = append(tracks, t)
	}
	conn.mu.Unlock()

	for _, t := range tracks {
		t.remote.delLocal(t)
	}
	return nil
}

func openDiskFile(directory, label string) (*os.File, error) {
	filename := time.Now().Format("2006-01-02T15:04:05")
	if label != "" {
		filename = filename + "-" + label
	}
	for counter := 0; counter < 100; counter++ {
		var fn string
		if counter == 0 {
			fn = fmt.Sprintf("%v.webm", filename)
		} else {
			fn = fmt.Sprintf("%v-%02d.webm", filename, counter)
		}

		fn = filepath.Join(directory, fn)
		f, err := os.OpenFile(
			fn, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600,
		)
		if err == nil {
			return f, nil
		} else if !os.IsExist(err) {
			return nil, err
		}
	}
	return nil, errors.New("couldn't create file")
}

type diskTrack struct {
	remote *upTrack
	conn   *diskConn

	writer    webm.BlockWriteCloser
	builder   *samplebuilder.SampleBuilder
	timestamp uint32
}

func newDiskConn(directory, label string, up *upConnection, remoteTracks []*upTrack) (*diskConn, error) {
	conn := diskConn{
		directory: directory,
		label:     label,
		tracks:    make([]*diskTrack, 0, len(remoteTracks)),
		remote:    up,
	}
	video := false
	for _, remote := range remoteTracks {
		var builder *samplebuilder.SampleBuilder
		switch remote.track.Codec().Name {
		case webrtc.Opus:
			builder = samplebuilder.New(16, &codecs.OpusPacket{})
		case webrtc.VP8:
			if video {
				return nil, errors.New("multiple video tracks not supported")
			}
			builder = samplebuilder.New(32, &codecs.VP8Packet{})
			video = true
		}
		track := &diskTrack{
			remote:  remote,
			builder: builder,
			conn:    &conn,
		}
		conn.tracks = append(conn.tracks, track)
		remote.addLocal(track)
	}

	if !video {
		err := conn.initWriter(0, 0)
		if err != nil {
			return nil, err
		}
	}

	err := up.addLocal(&conn)
	if err != nil {
		return nil, err
	}

	return &conn, nil
}

func clonePacket(packet *rtp.Packet) *rtp.Packet {
	buf, err := packet.Marshal()
	if err != nil {
		return nil
	}
	var p rtp.Packet
	err = p.Unmarshal(buf)
	if err != nil {
		return nil
	}
	return &p
}

func (t *diskTrack) WriteRTP(packet *rtp.Packet) error {
	// since we call initWriter, we take the connection lock for simplicity.
	t.conn.mu.Lock()
	defer t.conn.mu.Unlock()

	if t.builder == nil {
		return nil
	}

	p := clonePacket(packet)
	if p == nil {
		return nil
	}

	t.builder.Push(p)

	for {
		sample := t.builder.Pop()
		if sample == nil {
			return nil
		}

		t.timestamp += sample.Samples

		keyframe := true

		switch t.remote.track.Codec().Name {
		case webrtc.VP8:
			if len(sample.Data) < 1 {
				return nil
			}
			keyframe = (sample.Data[0]&0x1 == 0)
			if keyframe {
				err := t.initWriter(sample.Data)
				if err != nil {
					return err
				}
			}
		}
		if t.writer == nil {
			if !keyframe {
				return ErrKeyframeNeeded
			}
			return nil
		}

		tm := t.timestamp / (t.remote.track.Codec().ClockRate / 1000)
		_, err := t.writer.Write(keyframe, int64(tm), sample.Data)
		if err != nil {
			return err
		}
	}
}

// called locked
func (t *diskTrack) initWriter(data []byte) error {
	switch t.remote.track.Codec().Name {
	case webrtc.VP8:
		if len(data) < 10 {
			return nil
		}
		keyframe := (data[0]&0x1 == 0)
		if !keyframe {
			return nil
		}
		raw := uint32(data[6]) | uint32(data[7])<<8 |
			uint32(data[8])<<16 | uint32(data[9])<<24
		width := raw & 0x3FFF
		height := (raw >> 16) & 0x3FFF
		return t.conn.initWriter(width, height)
	}
	return nil
}

// called locked
func (conn *diskConn) initWriter(width, height uint32) error {
	if conn.file != nil && width == conn.width && height == conn.height {
		return nil
	}
	var entries []webm.TrackEntry
	for i, t := range conn.tracks {
		codec := t.remote.track.Codec()
		var entry webm.TrackEntry
		switch t.remote.track.Codec().Name {
		case webrtc.Opus:
			entry = webm.TrackEntry{
				Name:        "Audio",
				TrackNumber: uint64(i + 1),
				CodecID:     "A_OPUS",
				TrackType:   2,
				Audio: &webm.Audio{
					SamplingFrequency: float64(codec.ClockRate),
					Channels:          uint64(codec.Channels),
				},
			}
		case webrtc.VP8:
			entry = webm.TrackEntry{
				Name:        "Video",
				TrackNumber: uint64(i + 1),
				CodecID:     "V_VP8",
				TrackType:   1,
				Video: &webm.Video{
					PixelWidth:  uint64(width),
					PixelHeight: uint64(height),
				},
			}
		default:
			return errors.New("unknown track type")
		}
		entries = append(entries, entry)
	}

	err := conn.reopen()
	if err != nil {
		return err
	}

	writers, err := webm.NewSimpleBlockWriter(conn.file, entries)
	if err != nil {
		conn.file.Close()
		conn.file = nil
		return err
	}

	if len(writers) != len(conn.tracks) {
		conn.file.Close()
		conn.file = nil
		return errors.New("unexpected number of writers")
	}

	conn.width = width
	conn.height = height

	for i, t := range conn.tracks {
		t.writer = writers[i]
	}
	return nil
}

func (t *diskTrack) Accumulate(bytes uint32) {
	return
}

func (down *diskTrack) GetMaxBitrate(now uint64) uint64 {
	return ^uint64(0)
}
