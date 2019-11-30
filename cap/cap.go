// Package cap is the Linux capabilities user space API (libcap)
// bindings in native Go. The key advantage is no requirement for cgo
// linking etc. which can enable striaghtforward cross-compilation of
// different architectures just the Go compiler.
package cap

import (
	"errors"
	"sort"
	"sync"
	"syscall"
	"unsafe"
)

// Value is the type of a single capability (or permission) bit.
type Value uint

// Flag is the type of one of the three Value vectors held in a Set.
type Flag uint

// Effective, Permitted, Inheritable are the three vectors of Values
// held in a Set.
const (
	Effective Flag = iota
	Permitted
	Inheritable
)

// data holds a 32-bit slice of the compressed bitmaps of capability
// sets as understood by the kernel.
type data [Inheritable + 1]uint32

// Set is an opaque capabilities container for a set of system
// capbilities.
type Set struct {
	// mu protects all other members of a Set.
	mu sync.RWMutex

	// flat holds Flag Value bitmaps for all capabilities
	// associated with this Set.
	flat []data

	// Linux specific
	nsRoot int
}

// Various known kernel magic values.
const (
	kv1 = 0x19980330 // First iteration of process capabilities (32 bits).
	kv2 = 0x20071026 // First iteration of process and file capabilities (64 bits) - deprecated.
	kv3 = 0x20080522 // Most recently supported process and file capabilities (64 bits).
)

var (
	// starUp protects setting of the following values: magic,
	// words, maxValues.
	startUp sync.Once

	// magic holds the preferred magic number for the kernel ABI.
	magic uint32

	// words holds the number of uint32's associated with each
	// capability vector for this session.
	words int

	// maxValues holds the number of bit values that are named by
	// the running kernel. This is generally expected to match
	// ValueCount which is autogenerated at packaging time.
	maxValues uint
)

type header struct {
	magic uint32
	pid   int32
}

// caprcall provides a pointer etc wrapper for the system calls
// associated with getcap.
func caprcall(call uintptr, h *header, d []data) error {
	x := uintptr(0)
	if d != nil {
		x = uintptr(unsafe.Pointer(&d[0]))
	}
	_, _, err := callRKernel(call, uintptr(unsafe.Pointer(h)), x, 0)
	if err != 0 {
		return err
	}
	return nil
}

// capwcall provides a pointer etc wrapper for the system calls
// associated with setcap.
func capwcall(call uintptr, h *header, d []data) error {
	x := uintptr(0)
	if d != nil {
		x = uintptr(unsafe.Pointer(&d[0]))
	}
	_, _, err := callWKernel(call, uintptr(unsafe.Pointer(h)), x, 0)
	if err != 0 {
		return err
	}
	return nil
}

// prctlrcall provides a wrapper for the prctl systemcalls that only
// read kernel state. There is a limited number of arguments needed
// and the caller should use 0 for those not needed.
func prctlrcall(prVal, v1, v2 uintptr) (int, error) {
	r, _, err := callRKernel(syscall.SYS_PRCTL, prVal, v1, v2)
	if err != 0 {
		return int(r), err
	}
	return int(r), nil
}

// prctlwcall provides a wrapper for the prctl systemcalls that
// write/modify kernel state (where available, these will use the
// POSIX semantics fixup system calls). There is a limited number of
// arguments needed and the caller should use 0 for those not needed.
func prctlwcall(prVal, v1, v2 uintptr) (int, error) {
	r, _, err := callWKernel(syscall.SYS_PRCTL, prVal, v1, v2)
	if err != 0 {
		return int(r), err
	}
	return int(r), nil
}

// cInit perfoms the lazy identification of the capability vintage of
// the running system.
func cInit() {
	h := &header{
		magic: kv3,
	}
	caprcall(syscall.SYS_CAPGET, h, nil)
	magic = h.magic
	switch magic {
	case kv1:
		words = 1
	case kv2, kv3:
		words = 2
	default:
		// Fall back to a known good version.
		magic = kv3
		words = 2
	}
	// Use the bounding set to evaluate which capabilities exist.
	maxValues = uint(sort.Search(32*words, func(n int) bool {
		_, err := GetBound(Value(n))
		return err != nil
	}))
	if maxValues == 0 {
		// Fall back to using the largest value defined at build time.
		maxValues = NamedCount
	}
}

// NewSet returns an empty capability set.
func NewSet() *Set {
	startUp.Do(cInit)
	return &Set{
		flat: make([]data, words),
	}
}

// ErrBadSet indicates a nil pointer was used for a *Set, or the
// request of the Set is invalid in some way.
var ErrBadSet = errors.New("bad capability set")

// Dup returns a copy of the specified capability set.
func (c *Set) Dup() (*Set, error) {
	if c == nil || len(c.flat) == 0 {
		return nil, ErrBadSet
	}
	n := NewSet()
	c.mu.RLock()
	defer c.mu.RUnlock()
	copy(n.flat, c.flat)
	n.nsRoot = c.nsRoot
	return n, nil
}

// ErrBadValue indicates a bad capability value was specified.
var ErrBadValue = errors.New("bad capability value")

// bitOf convertes from a Value into the offset and mask for a
// specific Value bit in the compressed (kernel ABI) representation of
// a capability vector. If the requested bit is unsupported, an error
// is returned.
func bitOf(vec Flag, val Value) (uint, uint32, error) {
	if vec > Inheritable || val > Value(words*32) {
		return 0, 0, ErrBadValue
	}
	u := uint(val)
	return u / 32, uint32(1) << (u % 32), nil
}

// GetFlag determines if the requested bit is enabled in the Flag
// vector of the capability Set.
func (c *Set) GetFlag(vec Flag, val Value) (bool, error) {
	if c == nil || len(c.flat) == 0 {
		// Checked this first, because otherwise we are sure
		// cInit has been called.
		return false, ErrBadSet
	}
	offset, mask, err := bitOf(vec, val)
	if err != nil {
		return false, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.flat[offset][vec]&mask != 0, nil
}

// SetFlag sets the requested bits to the indicated enable state. This
// function does not perform any security checks, so values can be set
// out-of-order. Only when the Set is used to SetProc() etc., will the
// bits be checked for validity and permission by the kernel. If the
// function returns an error, the Set will not be modified.
func (c *Set) SetFlag(vec Flag, enable bool, val ...Value) error {
	if c == nil || len(c.flat) == 0 {
		// Checked this first, because otherwise we are sure
		// cInit has been called.
		return ErrBadSet
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Make a backup.
	replace := make([]uint32, words)
	for i := range replace {
		replace[i] = c.flat[i][vec]
	}
	var err error
	for _, v := range val {
		offset, mask, err2 := bitOf(vec, v)
		if err2 != nil {
			err = err2
			break
		}
		if enable {
			c.flat[offset][vec] |= mask
		} else {
			c.flat[offset][vec] &= ^mask
		}
	}
	if err == nil {
		return nil
	}
	// Clean up.
	for i, bits := range replace {
		c.flat[i][vec] = bits
	}
	return err
}

// Clear fully clears a capability set.
func (c *Set) Clear() error {
	if c == nil || len(c.flat) == 0 {
		return ErrBadSet
	}
	// startUp.Do(cInit) is not called here because c cannot be
	// initialized except via this package and doing that will
	// perform that call at least once (sic).
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flat = make([]data, words)
	c.nsRoot = 0
	return nil
}

// forceFlag sets all capability values of a flag vector to enable.
func (c *Set) forceFlag(vec Flag, enable bool) error {
	if c == nil || len(c.flat) == 0 || vec > Inheritable {
		return ErrBadSet
	}
	m := uint32(0)
	if enable {
		m = ^m
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.flat {
		c.flat[i][vec] = m
	}
	return nil
}

// ClearFlag clears a specific vector of Values associated with the
// specified Flag.
func (c *Set) ClearFlag(vec Flag) error {
	return c.forceFlag(vec, false)
}

// GetPID returns the capability set associated with the target process
// id; pid=0 is an alias for current.
func GetPID(pid int) (*Set, error) {
	v := NewSet()
	if err := caprcall(syscall.SYS_CAPGET, &header{magic: magic, pid: int32(pid)}, v.flat); err != nil {
		return nil, err
	}
	return v, nil
}

// GetProc returns the capability Set of the current process. If the
// kernel is unable to determine the Set associated with the current
// process, the function panic()s.
func GetProc() *Set {
	c, err := GetPID(0)
	if err != nil {
		panic(err)
	}
	return c
}

// SetProc attempts to write the capability Set to the current
// process. The kernel will perform permission checks and an error
// will be returned if the attempt fails.
func (c *Set) SetProc() error {
	if c == nil || len(c.flat) == 0 {
		return ErrBadSet
	}
	return capwcall(syscall.SYS_CAPSET, &header{magic: magic}, c.flat)
}

// defines from uapi/linux/prctl.h
const (
	PR_CAPBSET_READ = 23
	PR_CAPBSET_DROP = 24
)

// GetBound determines if a specific capability is currently part of
// the local bounding set. On systems where the bounding set Value is
// not present, this function returns an error.
func GetBound(val Value) (bool, error) {
	v, err := prctlrcall(PR_CAPBSET_READ, uintptr(val), 0)
	if err != nil {
		return false, err
	}
	return v > 0, nil
}

// DropBound attempts to suppress bounding set Values. The kernel will
// never allow a bounding set Value bit to be raised once successfully
// dropped. However, dropping requires the current process is
// sufficiently capable (usually via cap.SETPCAP being raised in the
// Effective flag vector). Note, the drops are performed in order and
// if one bounding value cannot be dropped, the function returns
// immediately with an error which may leave the system in an
// ill-defined state.
func DropBound(val ...Value) error {
	for _, v := range val {
		if _, err := prctlwcall(PR_CAPBSET_DROP, uintptr(v), 0); err != nil {
			return err
		}
	}
	return nil
}

// defines from uapi/linux/prctl.h
const (
	PR_CAP_AMBIENT = 47

	PR_CAP_AMBIENT_IS_SET    = 1
	PR_CAP_AMBIENT_RAISE     = 2
	PR_CAP_AMBIENT_LOWER     = 3
	PR_CAP_AMBIENT_CLEAR_ALL = 4
)

// GetAmbient determines if a specific capability is currently part of
// the local ambient set. On systems where the ambient set Value is
// not present, this function returns an error.
func GetAmbient(val Value) (bool, error) {
	r, err := prctlrcall(PR_CAP_AMBIENT, PR_CAP_AMBIENT_IS_SET, uintptr(val))
	return r > 0, err
}

// SetAmbient attempts to set a specific Value bit to the enable
// state. This function will return an error if insufficient
// permission is available to perform this task. The settings are
// performed in order and the function returns immediately an error is
// detected.
func SetAmbient(enable bool, val ...Value) error {
	dir := uintptr(PR_CAP_AMBIENT_LOWER)
	if enable {
		dir = PR_CAP_AMBIENT_RAISE
	}
	for _, v := range val {
		_, err := prctlwcall(PR_CAP_AMBIENT, dir, uintptr(v))
		if err != nil {
			return err
		}
	}
	return nil
}

// ResetAmbient attempts to fully clear the Ambient set.
func ResetAmbient() error {
	_, err := prctlwcall(PR_CAP_AMBIENT, PR_CAP_AMBIENT_CLEAR_ALL, 0)
	return err
}
