// exp23 microbenchmark: AES-128-GCM seal/open cost for DTLS-record-sized
// records.  Stdlib only.  Reports CPU-s/GB so the number is directly
// comparable with the harness' go_cpu_seconds/(recv/1e9) figure.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
)

func cpuSeconds() float64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 +
		float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

func main() {
	rec := flag.Int("rec", 1140, "record payload bytes (DTLS record over a 1280B MTU)")
	n := flag.Int("n", 4_000_000, "records")
	flag.Parse()

	key := make([]byte, 16)
	_, _ = rand.Read(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	nonce := make([]byte, aead.NonceSize())
	plain := make([]byte, *rec)
	sealed := make([]byte, 0, *rec+aead.Overhead())
	dst := make([]byte, *rec+aead.Overhead())

	runtime.GC()
	c0 := cpuSeconds()
	t0 := time.Now()
	var total int64
	for i := 0; i < *n; i++ {
		sealed = aead.Seal(sealed[:0], nonce, plain, nil)
		total += int64(len(sealed))
		// open path (receiver side, same cipher cost)
		if _, err := aead.Open(dst[:0], nonce, sealed, nil); err != nil {
			panic(err)
		}
	}
	wall := time.Since(t0)
	cpu := cpuSeconds() - c0
	gb := float64(total) / 1e9
	fmt.Fprintf(os.Stderr, "aes128gcm rec=%dB records=%d bytes=%.3fGB wall=%.3fs cpu=%.3fs\n",
		*rec, *n, gb, wall.Seconds(), cpu)
	fmt.Printf("{\"bench\":\"aesgcm\",\"rec\":%d,\"bytes\":%.0f,\"wall_s\":%.4f,\"cpu_s\":%.4f,\"seal_open_cpu_s_per_gb\":%.3f,\"seal_only_cpu_s_per_gb\":%.3f,\"seal_only_gbps_per_core\":%.2f}\n",
		*rec, float64(total), wall.Seconds(), cpu, cpu/gb, cpu/gb/2, gb/(cpu/2))
}
