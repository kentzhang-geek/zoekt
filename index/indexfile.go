// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package index

import (
	"fmt"
	"log"
	"os"
    "syscall"
    "unsafe"

)

type mmapedIndexFile struct {
	name string
	size uint32
    hMap syscall.Handle
	data []byte
}

func (f *mmapedIndexFile) Read(off, sz uint32) ([]byte, error) {
	if off > off+sz || off+sz > uint32(len(f.data)) {
		return nil, fmt.Errorf("out of bounds: %d, len %d, name %s", off+sz, len(f.data), f.name)
	}
	return f.data[off : off+sz], nil
}

func (f *mmapedIndexFile) Name() string {
	return f.name
}

func (f *mmapedIndexFile) Size() (uint32, error) {
	return f.size, nil
}

func (f *mmapedIndexFile) Close() {
    if err := syscall.UnmapViewOfFile(uintptr(unsafe.Pointer(&f.data[0]))); err != nil {
		log.Printf("WARN failed to Munmap %s: %v", f.name, err)
    }
    if err := syscall.CloseHandle(f.hMap); err != nil {
		log.Printf("WARN failed to Munmap %s: %v", f.name, err)
    }
}

// NewIndexFile returns a new index file. The index file takes
// ownership of the passed in file, and may close it.
func NewIndexFile(f *os.File) (IndexFile, error) {
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	sz := fi.Size()
	if sz >= maxUInt32 {
		return nil, fmt.Errorf("file %s too large: %d", f.Name(), sz)
	}
	r := &mmapedIndexFile{
		name: f.Name(),
		size: uint32(sz),
	}

    hMap, err := syscall.CreateFileMapping(syscall.Handle(f.Fd()), nil, syscall.PAGE_READONLY, 0, uint32(r.size), nil)
    if err != nil {
        return nil, err
    }
    r.hMap = hMap
    addr, err := syscall.MapViewOfFile(r.hMap, syscall.FILE_MAP_READ, 0, 0, uintptr(r.size))
    if err != nil {
        syscall.CloseHandle(hMap)
        return nil, err
    }

    r.data = (*[1 << 30]byte)(unsafe.Pointer(addr))[:r.size]
	if err != nil {
		return nil, err
	}

	return r, err
}
