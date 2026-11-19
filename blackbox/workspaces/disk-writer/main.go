package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
)

func main() {
	bytes, err := strconv.ParseInt(os.Getenv("VMJAIL_WRITE_BYTES"), 10, 64)
	if err != nil || bytes < 0 {
		panic("VMJAIL_WRITE_BYTES must be a non-negative integer")
	}
	path := os.Getenv("VMJAIL_WRITE_PATH")
	if path == "" {
		path = "payload.bin"
	}
	file, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	written, copyErr := io.CopyN(file, zeroReader{}, bytes)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		panic(fmt.Sprintf("write=%v close=%v", copyErr, closeErr))
	}
	fmt.Printf("wrote %d bytes\n", written)
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}
