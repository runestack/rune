// uffd-handler serves one Firecracker microVM's guest memory from a snapshot
// memory file, for the "Uffd" memory backend (E2's second arm, and E5's lazy
// load). It follows the protocol of Firecracker's own example handler
// (src/firecracker/examples/uffd/uffd_utils.rs):
//
//  1. Listen on a unix socket; Firecracker connects during /snapshot/load.
//  2. Receive one message: a JSON array of guest regions plus the
//     userfaultfd as SCM_RIGHTS.
//  3. Serve each page fault with UFFDIO_COPY from a MAP_PRIVATE mapping of
//     the memory file. EEXIST means another fault already filled the page;
//     EAGAIN means a REMOVE event is queued, so drain events and retry.
//  4. After a balloon REMOVE, faults in that range get zero pages, not the
//     snapshot's stale contents.
//
// Note: UFFDIO_COPY gives every VM its own private copy of each page it
// touches. Sharing clean pages between siblings comes from the File backend
// (MAP_PRIVATE of one mem file), not from this handler. UFFD's job is lazy
// and remote loading.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux uapi constants (include/uapi/linux/userfaultfd.h); x/sys has none.
const (
	uffdEventPagefault = 0x12
	uffdEventFork      = 0x13
	uffdEventRemap     = 0x14
	uffdEventRemove    = 0x15
	uffdEventUnmap     = 0x16

	uffdioWake     = 0x8010aa02 // _IOR(0xAA, 0x02, struct uffdio_range)
	uffdioCopy     = 0xc028aa03 // _IOWR(0xAA, 0x03, struct uffdio_copy)
	uffdioZeropage = 0xc020aa04 // _IOWR(0xAA, 0x04, struct uffdio_zeropage)

	uffdMsgSize = 32
)

type region struct {
	Base       uint64 `json:"base_host_virt_addr"`
	Size       uint64 `json:"size"`
	Offset     uint64 `json:"offset"`
	PageSize   uint64 `json:"page_size"`
	PageSizeKB uint64 `json:"page_size_kib"` // older Firecracker releases
}

type uffdioCopyArg struct {
	Dst, Src, Len, Mode uint64
	Copy                int64
}

type uffdioZeropageArg struct {
	Start, Len, Mode uint64
	Zeropage         int64
}

type stats struct {
	Faults      atomic.Uint64
	Copied      atomic.Uint64
	Zeroed      atomic.Uint64
	Exists      atomic.Uint64
	Retries     atomic.Uint64
	Removes     atomic.Uint64
	FirstFaultT atomic.Int64
}

func main() {
	sock := flag.String("sock", "", "unix socket path Firecracker connects to (mem_backend.backend_path)")
	memPath := flag.String("mem", "", "snapshot memory file")
	populate := flag.Bool("populate", false, "MAP_POPULATE the memory file up front")
	owner := flag.String("owner", "", "uid:gid to chown the socket to (the jailer user)")
	statsPath := flag.String("stats", "", "write JSON fault statistics here on exit")
	flag.Parse()
	log.SetFlags(log.Lmicroseconds)
	if *sock == "" || *memPath == "" {
		flag.Usage()
		os.Exit(2)
	}

	src, err := mapFile(*memPath, *populate)
	if err != nil {
		log.Fatal(err)
	}

	_ = os.Remove(*sock)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: *sock, Net: "unix"})
	if err != nil {
		log.Fatal(err)
	}
	if *owner != "" {
		var uid, gid int
		if _, err := fmt.Sscanf(*owner, "%d:%d", &uid, &gid); err != nil {
			log.Fatalf("bad -owner %q: %v", *owner, err)
		}
		if err := os.Chown(*sock, uid, gid); err != nil {
			log.Fatal(err)
		}
	}

	st := &stats{}
	start := time.Now()
	writeStats := func() {
		if *statsPath == "" {
			return
		}
		b, _ := json.Marshal(map[string]any{
			"faults":            st.Faults.Load(),
			"copied_pages":      st.Copied.Load(),
			"zero_pages":        st.Zeroed.Load(),
			"eexist":            st.Exists.Load(),
			"eagain_retries":    st.Retries.Load(),
			"remove_events":     st.Removes.Load(),
			"first_fault_ns":    st.FirstFaultT.Load(),
			"handler_uptime_ns": time.Since(start).Nanoseconds(),
		})
		_ = os.WriteFile(*statsPath, b, 0o644)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		writeStats()
		os.Exit(0)
	}()

	conn, err := ln.AcceptUnix()
	if err != nil {
		log.Fatal(err)
	}
	ln.Close()
	regions, uffd, err := receive(conn)
	if err != nil {
		log.Fatal(err)
	}
	var total uint64
	for _, r := range regions {
		total = max(total, r.Offset+r.Size)
	}
	if total > uint64(len(src)) {
		log.Fatalf("regions need %d bytes but %s has %d", total, *memPath, len(src))
	}
	h := &handler{uffd: uffd, regions: regions, src: src, st: st, start: start, removed: map[uint64]bool{}}
	h.loop()
	writeStats()
}

func mapFile(path string, populate bool) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	flags := unix.MAP_PRIVATE
	if populate {
		flags |= unix.MAP_POPULATE
	}
	b, err := unix.Mmap(int(f.Fd()), 0, int(fi.Size()), unix.PROT_READ, flags)
	if err != nil {
		return nil, fmt.Errorf("mmap %s: %w", path, err)
	}
	return b, nil
}

// receive reads Firecracker's one message: the region JSON with the uffd
// attached as SCM_RIGHTS.
func receive(conn *net.UnixConn) ([]region, int, error) {
	buf := make([]byte, 64<<10)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, -1, fmt.Errorf("recvmsg: %w", err)
	}
	cmsgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(cmsgs) == 0 {
		return nil, -1, fmt.Errorf("no control message from firecracker (err=%v)", err)
	}
	fds, err := unix.ParseUnixRights(&cmsgs[0])
	if err != nil || len(fds) == 0 {
		return nil, -1, fmt.Errorf("no uffd in control message (err=%v)", err)
	}
	var regions []region
	if err := json.Unmarshal(buf[:n], &regions); err != nil {
		return nil, -1, fmt.Errorf("region JSON %q: %w", buf[:n], err)
	}
	if len(regions) == 0 {
		return nil, -1, errors.New("firecracker sent no regions")
	}
	for i := range regions {
		if regions[i].PageSize == 0 {
			regions[i].PageSize = regions[i].PageSizeKB * 1024
		}
		if ps := regions[i].PageSize; ps == 0 || ps&(ps-1) != 0 {
			return nil, -1, fmt.Errorf("region %d: bad page size %d", i, ps)
		}
	}
	return regions, fds[0], nil
}

type handler struct {
	uffd     int
	regions  []region
	src      []byte
	st       *stats
	start    time.Time
	removed  map[uint64]bool // page addresses dropped by the balloon
	deferred []uint64        // faults that hit EAGAIN, retried after events drain
}

func (h *handler) loop() {
	msg := make([]byte, uffdMsgSize*64)
	pfd := []unix.PollFd{{Fd: int32(h.uffd), Events: unix.POLLIN}}
	for {
		if _, err := unix.Poll(pfd, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			log.Fatalf("poll: %v", err)
		}
		if pfd[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0 {
			return // Firecracker exited
		}
		n, err := unix.Read(h.uffd, msg)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			log.Fatalf("read uffd: %v", err)
		}
		if n == 0 {
			return
		}
		// Apply REMOVEs first so faults in a removed range are served as zero.
		for off := 0; off+uffdMsgSize <= n; off += uffdMsgSize {
			m := msg[off : off+uffdMsgSize]
			if m[0] == uffdEventRemove {
				h.remove(binary.NativeEndian.Uint64(m[8:]), binary.NativeEndian.Uint64(m[16:]))
			}
		}
		for off := 0; off+uffdMsgSize <= n; off += uffdMsgSize {
			m := msg[off : off+uffdMsgSize]
			switch m[0] {
			case uffdEventPagefault:
				h.fault(binary.NativeEndian.Uint64(m[16:]))
			case uffdEventRemove:
			case uffdEventFork, uffdEventRemap, uffdEventUnmap:
				log.Printf("ignoring uffd event 0x%x", m[0])
			default:
				log.Fatalf("unexpected uffd event 0x%x", m[0])
			}
		}
		pending := h.deferred
		h.deferred = nil
		for _, addr := range pending {
			h.fault(addr)
		}
	}
}

func (h *handler) find(addr uint64) *region {
	for i := range h.regions {
		r := &h.regions[i]
		if addr >= r.Base && addr < r.Base+r.Size {
			return r
		}
	}
	return nil
}

func (h *handler) remove(start, end uint64) {
	h.st.Removes.Add(1)
	r := h.find(start)
	if r == nil {
		return
	}
	for a := start &^ (r.PageSize - 1); a < end; a += r.PageSize {
		h.removed[a] = true
	}
}

func (h *handler) fault(addr uint64) {
	if h.st.Faults.Add(1) == 1 {
		h.st.FirstFaultT.Store(time.Since(h.start).Nanoseconds())
	}
	r := h.find(addr)
	if r == nil {
		log.Fatalf("fault at 0x%x outside every guest region", addr)
	}
	page := addr &^ (r.PageSize - 1)
	var errno syscall.Errno
	if h.removed[page] {
		arg := uffdioZeropageArg{Start: page, Len: r.PageSize}
		_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(h.uffd), uffdioZeropage, uintptr(unsafe.Pointer(&arg)))
		if errno == 0 {
			h.st.Zeroed.Add(1)
		}
	} else {
		off := r.Offset + (page - r.Base)
		arg := uffdioCopyArg{Dst: page, Src: uint64(uintptr(unsafe.Pointer(&h.src[off]))), Len: r.PageSize}
		_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(h.uffd), uffdioCopy, uintptr(unsafe.Pointer(&arg)))
		if errno == 0 {
			h.st.Copied.Add(1)
		}
	}
	switch errno {
	case 0:
	case unix.EEXIST:
		// Already populated by a racing fault; make sure the faulting thread wakes.
		h.st.Exists.Add(1)
		wake := [2]uint64{page, r.PageSize}
		unix.Syscall(unix.SYS_IOCTL, uintptr(h.uffd), uffdioWake, uintptr(unsafe.Pointer(&wake)))
	case unix.EAGAIN:
		h.st.Retries.Add(1)
		h.deferred = append(h.deferred, addr)
	default:
		log.Fatalf("serving fault at 0x%x: %v", addr, errno)
	}
}
