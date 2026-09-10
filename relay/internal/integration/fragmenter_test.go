package integration

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// TestRecordFragmenterEmitsExactlyRequestedRecordCount proves the record
// fragmenter splits the first handshake record into EXACTLY the requested
// number of TLS records for every body length. The §23.3 gate asserts an
// exact record count, so a splitter that sometimes emits fewer chunks would
// make that assertion unsound (and the old `>= 3` gate weak).
func TestRecordFragmenterEmitsExactlyRequestedRecordCount(t *testing.T) {
	for _, parts := range []int{2, 3, 5} {
		for _, bodyLen := range []int{parts, parts + 1, 11, 17, 23, 40, 64, 101, 203} {
			if bodyLen < parts {
				continue
			}
			records, received, err := fragmentHandshakeRecord(bodyLen, parts)
			if err != nil {
				t.Errorf("bodyLen=%d parts=%d: fragment: %v", bodyLen, parts, err)
				continue
			}
			if records != parts {
				t.Errorf("bodyLen=%d parts=%d: fragmenter emitted %d TLS record(s), want exactly %d",
					bodyLen, parts, records, parts)
			}
			wantBody := make([]byte, bodyLen)
			for i := range wantBody {
				wantBody[i] = byte(i)
			}
			payload, payloadErr := tlsRecordPayload(received)
			if payloadErr != nil {
				t.Errorf("bodyLen=%d parts=%d: parse fragmented records: %v", bodyLen, parts, payloadErr)
				continue
			}
			if !bytes.Equal(payload, wantBody) {
				t.Errorf("bodyLen=%d parts=%d: reassembled record payload differs from the original body", bodyLen, parts)
			}
		}
	}
}

// fragmentHandshakeRecord writes one synthetic TLS handshake record through a
// real recordFragmenter over a loopback socket and returns the number of TLS
// records and the exact bytes the peer received.
func fragmentHandshakeRecord(bodyLen, parts int) (int, []byte, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, nil, err
	}
	defer listener.Close()

	type readResult struct {
		data []byte
		err  error
	}
	results := make(chan readResult, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			results <- readResult{err: acceptErr}
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		data, readErr := io.ReadAll(conn)
		results <- readResult{data: data, err: readErr}
	}()

	raw, err := net.DialTimeout("tcp", listener.Addr().String(), 5*time.Second)
	if err != nil {
		return 0, nil, err
	}
	fragmenter := &recordFragmenter{conn: raw, parts: parts}
	record := make([]byte, 5+bodyLen)
	record[0], record[1], record[2] = 0x16, 0x03, 0x03
	binary.BigEndian.PutUint16(record[3:5], uint16(bodyLen))
	for i := range record[5:] {
		record[5+i] = byte(i)
	}
	if _, err := fragmenter.Write(record); err != nil {
		raw.Close()
		return 0, nil, err
	}
	_ = raw.Close()

	result := <-results
	if result.err != nil {
		return 0, nil, result.err
	}
	records, countErr := countTLSRecords(result.data)
	return records, result.data, countErr
}

// countTLSRecords counts the TLS records in a raw handshake byte stream.
func countTLSRecords(stream []byte) (int, error) {
	records := 0
	for offset := 0; offset < len(stream); {
		if offset+5 > len(stream) {
			return records, io.ErrUnexpectedEOF
		}
		if stream[offset] != 0x16 {
			return records, io.ErrUnexpectedEOF
		}
		length := int(stream[offset+3])<<8 | int(stream[offset+4])
		offset += 5 + length
		records++
	}
	return records, nil
}

// tlsRecordPayload concatenates the bodies of every record in a raw TLS
// byte stream, reconstructing the handshake message the records carry.
func tlsRecordPayload(stream []byte) ([]byte, error) {
	var payload []byte
	for offset := 0; offset < len(stream); {
		if offset+5 > len(stream) {
			return nil, io.ErrUnexpectedEOF
		}
		length := int(stream[offset+3])<<8 | int(stream[offset+4])
		if offset+5+length > len(stream) {
			return nil, io.ErrUnexpectedEOF
		}
		payload = append(payload, stream[offset+5:offset+5+length]...)
		offset += 5 + length
	}
	return payload, nil
}
