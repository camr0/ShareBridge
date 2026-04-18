package transport

import (
	"os"
	"reflect"
	"strings"
	"time"
	"unsafe"
)

// YamuxDebugState exposes best-effort transport diagnostics for debugging.
// The values are derived from the current go-yamux internals and may become
// unavailable if upstream field layouts change.
type YamuxDebugState struct {
	StreamID   uint32
	SendWindow uint32
	RecvWindow uint32
	RTT        time.Duration
}

// DebugTransportEnabled reports whether transport-level diagnostics should be
// logged for the current process.
func DebugTransportEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SHAREBRIDGE_DEBUG_TRANSPORT"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// DebugYamuxState extracts best-effort yamux state from a libp2p stream.
func DebugYamuxState(stream any) (YamuxDebugState, bool) {
	return debugYamuxStateFromValue(reflect.ValueOf(stream))
}

func debugYamuxStateFromValue(v reflect.Value) (YamuxDebugState, bool) {
	inner, ok := debugField(v, "stream")
	if !ok {
		return YamuxDebugState{}, false
	}
	inner = debugDereference(inner)
	if !inner.IsValid() {
		return YamuxDebugState{}, false
	}

	sendWindow, ok := debugUint32Field(inner, "sendWindow")
	if !ok {
		return YamuxDebugState{}, false
	}
	recvWindow, ok := debugUint32Field(inner, "recvWindow")
	if !ok {
		return YamuxDebugState{}, false
	}
	streamID, ok := debugUint32Field(inner, "id")
	if !ok {
		return YamuxDebugState{}, false
	}

	session, ok := debugField(inner, "session")
	if !ok {
		return YamuxDebugState{}, false
	}
	session = debugDereference(session)
	if !session.IsValid() {
		return YamuxDebugState{}, false
	}

	rttNanos, ok := debugInt64Field(session, "rtt")
	if !ok {
		return YamuxDebugState{}, false
	}

	return YamuxDebugState{
		StreamID:   streamID,
		SendWindow: sendWindow,
		RecvWindow: recvWindow,
		RTT:        time.Duration(rttNanos),
	}, true
}

func debugUint32Field(v reflect.Value, name string) (uint32, bool) {
	field, ok := debugField(v, name)
	if !ok {
		return 0, false
	}
	field = debugDereference(field)
	if !field.IsValid() || field.Kind() != reflect.Uint32 {
		return 0, false
	}
	return uint32(field.Uint()), true
}

func debugInt64Field(v reflect.Value, name string) (int64, bool) {
	field, ok := debugField(v, name)
	if !ok {
		return 0, false
	}
	field = debugDereference(field)
	if !field.IsValid() || field.Kind() != reflect.Int64 {
		return 0, false
	}
	return field.Int(), true
}

func debugField(v reflect.Value, name string) (reflect.Value, bool) {
	v = debugDereference(v)
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	field := v.FieldByName(name)
	if !field.IsValid() {
		return reflect.Value{}, false
	}
	return debugExpose(field), true
}

func debugDereference(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
		v = debugExpose(v)
		if !v.IsValid() || v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

func debugExpose(v reflect.Value) reflect.Value {
	if !v.IsValid() || v.CanInterface() {
		return v
	}
	if !v.CanAddr() {
		return reflect.Value{}
	}
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
}
