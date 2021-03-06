package test

import (
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/hanwen/go-fuse/fuse/pathfs"
)

const mode uint32 = 0757

type testCase struct {
	tmpDir string
	orig   string
	mnt    string

	mountFile   string
	mountSubdir string
	origFile    string
	origSubdir  string
	tester      *testing.T
	state       *fuse.Server
	pathFs      *pathfs.PathNodeFs
	connector   *nodefs.FileSystemConnector
}

const testTtl = 100 * time.Millisecond

// Create and mount filesystem.
func NewTestCase(t *testing.T) *testCase {
	me := &testCase{}
	me.tester = t

	// Make sure system setting does not affect test.
	syscall.Umask(0)

	const name string = "hello.txt"
	const subdir string = "subdir"

	var err error
	me.tmpDir, err = ioutil.TempDir("", "go-fuse")
	if err != nil {
		t.Fatalf("TempDir failed: %v", err)
	}
	me.orig = me.tmpDir + "/orig"
	me.mnt = me.tmpDir + "/mnt"

	os.Mkdir(me.orig, 0700)
	os.Mkdir(me.mnt, 0700)

	me.mountFile = filepath.Join(me.mnt, name)
	me.mountSubdir = filepath.Join(me.mnt, subdir)
	me.origFile = filepath.Join(me.orig, name)
	me.origSubdir = filepath.Join(me.orig, subdir)

	var pfs pathfs.FileSystem
	pfs = pathfs.NewLoopbackFileSystem(me.orig)
	pfs = pathfs.NewLockingFileSystem(pfs)

	me.pathFs = pathfs.NewPathNodeFs(pfs, &pathfs.PathNodeFsOptions{
		ClientInodes: true})
	me.connector = nodefs.NewFileSystemConnector(me.pathFs,
		&nodefs.Options{
			EntryTimeout:    testTtl,
			AttrTimeout:     testTtl,
			NegativeTimeout: 0.0,
		})
	me.connector.SetDebug(VerboseTest())
	me.state, err = fuse.NewServer(
		me.connector.RawFS(), me.mnt, &fuse.MountOptions{SingleThreaded: true})
	if err != nil {
		t.Fatal("NewServer:", err)
	}

	me.state.SetDebug(VerboseTest())

	// Unthreaded, but in background.
	go me.state.Serve()

	me.state.WaitMount()
	return me
}

// Unmount and del.
func (tc *testCase) Cleanup() {
	err := tc.state.Unmount()
	if err != nil {
		tc.tester.Fatalf("Unmount failed: %v", err)
	}
	os.RemoveAll(tc.tmpDir)
}

func (tc *testCase) rootNode() *nodefs.Inode {
	return tc.pathFs.Root().Inode()
}

////////////////
// Tests.

func TestOpenUnreadable(t *testing.T) {
	ts := NewTestCase(t)
	defer ts.Cleanup()
	_, err := os.Open(ts.mnt + "/doesnotexist")
	if err == nil {
		t.Errorf("open non-existent should raise error")
	}
}

func TestReadThrough(t *testing.T) {
	ts := NewTestCase(t)
	defer ts.Cleanup()

	content := RandomData(125)
	err := ioutil.WriteFile(ts.origFile, content, 0700)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	err = os.Chmod(ts.mountFile, os.FileMode(mode))
	if err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}

	fi, err := os.Lstat(ts.mountFile)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	if uint32(fi.Mode().Perm()) != mode {
		t.Errorf("Wrong mode %o != %o", int(fi.Mode().Perm()), mode)
	}

	// Open (for read), read.
	f, err := os.Open(ts.mountFile)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer f.Close()

	var buf [1024]byte
	slice := buf[:]
	n, err := f.Read(slice)
	CompareSlices(t, slice[:n], content)
}

func TestRemove(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	contents := []byte{1, 2, 3}
	err := ioutil.WriteFile(tc.origFile, []byte(contents), 0700)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	err = os.Remove(tc.mountFile)
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	_, err = os.Lstat(tc.origFile)
	if err == nil {
		t.Errorf("Lstat() after delete should have generated error.")
	}
}

func TestWriteThrough(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	// Create (for write), write.
	f, err := os.OpenFile(tc.mountFile, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	defer f.Close()

	content := RandomData(125)
	n, err := f.Write(content)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(content) {
		t.Errorf("Write mismatch: %v of %v", n, len(content))
	}

	fi, err := os.Lstat(tc.origFile)
	if fi.Mode().Perm() != 0644 {
		t.Errorf("create mode error %o", fi.Mode()&0777)
	}

	f, err = os.Open(tc.origFile)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer f.Close()

	var buf [1024]byte
	slice := buf[:]
	n, err = f.Read(slice)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	CompareSlices(t, slice[:n], content)
}

func TestMkdirRmdir(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	// Mkdir/Rmdir.
	err := os.Mkdir(tc.mountSubdir, 0777)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	fi, err := os.Lstat(tc.origSubdir)
	if !fi.IsDir() {
		t.Errorf("Not a directory: %v", fi)
	}

	err = os.Remove(tc.mountSubdir)
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
}

func TestLinkCreate(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	content := RandomData(125)
	err := ioutil.WriteFile(tc.origFile, content, 0700)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	err = os.Mkdir(tc.origSubdir, 0777)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	// Link.
	mountSubfile := filepath.Join(tc.mountSubdir, "subfile")
	err = os.Link(tc.mountFile, mountSubfile)
	if err != nil {
		t.Fatalf("Link failed: %v", err)
	}

	var subStat, stat syscall.Stat_t
	err = syscall.Lstat(mountSubfile, &subStat)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	err = syscall.Lstat(tc.mountFile, &stat)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	if stat.Nlink != 2 {
		t.Errorf("Expect 2 links: %v", stat)
	}
	if stat.Ino != subStat.Ino {
		t.Errorf("Link succeeded, but inode numbers different: %v %v", stat.Ino, subStat.Ino)
	}
	readback, err := ioutil.ReadFile(mountSubfile)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	CompareSlices(t, readback, content)

	err = os.Remove(tc.mountFile)
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	_, err = ioutil.ReadFile(mountSubfile)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
}

// Deal correctly with hard links implied by matching client inode
// numbers.
func TestLinkExisting(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	c := RandomData(5)

	err := ioutil.WriteFile(tc.orig+"/file1", c, 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	err = os.Link(tc.orig+"/file1", tc.orig+"/file2")
	if err != nil {
		t.Fatalf("Link failed: %v", err)
	}

	var s1, s2 syscall.Stat_t
	err = syscall.Lstat(tc.mnt+"/file1", &s1)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	err = syscall.Lstat(tc.mnt+"/file2", &s2)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	if s1.Ino != s2.Ino {
		t.Errorf("linked files should have identical inodes %v %v", s1.Ino, s2.Ino)
	}

	back, err := ioutil.ReadFile(tc.mnt + "/file1")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	CompareSlices(t, back, c)
}

// Deal correctly with hard links implied by matching client inode
// numbers.
func TestLinkForget(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	c := "hello"

	err := ioutil.WriteFile(tc.orig+"/file1", []byte(c), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	err = os.Link(tc.orig+"/file1", tc.orig+"/file2")
	if err != nil {
		t.Fatalf("Link failed: %v", err)
	}

	var s1, s2 syscall.Stat_t
	err = syscall.Lstat(tc.mnt+"/file1", &s1)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	tc.pathFs.ForgetClientInodes()

	err = syscall.Lstat(tc.mnt+"/file2", &s2)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	if s1.Ino == s2.Ino {
		t.Error("After forget, we should not export links")
	}
}

func TestSymlink(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	t.Log("testing symlink/readlink.")
	contents := []byte{1, 2, 3}
	err := ioutil.WriteFile(tc.origFile, []byte(contents), 0700)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	linkFile := "symlink-file"
	orig := "hello.txt"
	err = os.Symlink(orig, filepath.Join(tc.mnt, linkFile))

	if err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	origLink := filepath.Join(tc.orig, linkFile)
	fi, err := os.Lstat(origLink)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("not a symlink: %v", fi)
		return
	}

	read, err := os.Readlink(filepath.Join(tc.mnt, linkFile))
	if err != nil {
		t.Fatalf("Readlink failed: %v", err)
	}

	if read != orig {
		t.Errorf("unexpected symlink value '%v'", read)
	}
}

func TestRename(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	contents := []byte{1, 2, 3}
	err := ioutil.WriteFile(tc.origFile, []byte(contents), 0700)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	sd := tc.mnt + "/testRename"
	err = os.MkdirAll(sd, 0777)

	subFile := sd + "/subfile"
	err = os.Rename(tc.mountFile, subFile)
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}
	f, _ := os.Lstat(tc.origFile)
	if f != nil {
		t.Errorf("original %v still exists.", tc.origFile)
	}
	f, _ = os.Lstat(subFile)
	if f == nil {
		t.Errorf("destination %v does not exist.", subFile)
	}
}

// Flaky test, due to rename race condition.
func TestDelRename(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	t.Log("Testing del+rename.")

	sd := tc.mnt + "/testDelRename"
	err := os.MkdirAll(sd, 0755)
	if err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	d := sd + "/dest"
	err = ioutil.WriteFile(d, []byte("blabla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	f, err := os.Open(d)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer f.Close()

	err = os.Remove(d)
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	s := sd + "/src"
	err = ioutil.WriteFile(s, []byte("blabla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	err = os.Rename(s, d)
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}
}

func TestOverwriteRename(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	t.Log("Testing rename overwrite.")

	sd := tc.mnt + "/testOverwriteRename"
	err := os.MkdirAll(sd, 0755)
	if err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	d := sd + "/dest"
	err = ioutil.WriteFile(d, []byte("blabla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	s := sd + "/src"
	err = ioutil.WriteFile(s, []byte("blabla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	err = os.Rename(s, d)
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}
}

func TestAccess(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Log("Skipping TestAccess() as root.")
		return
	}
	tc := NewTestCase(t)
	defer tc.Cleanup()

	contents := []byte{1, 2, 3}
	err := ioutil.WriteFile(tc.origFile, []byte(contents), 0700)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	err = os.Chmod(tc.origFile, 0)
	if err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}
	// Ugh - copied from unistd.h
	const W_OK uint32 = 2

	errCode := syscall.Access(tc.mountFile, W_OK)
	if errCode != syscall.EACCES {
		t.Errorf("Expected EACCES for non-writable, %v %v", errCode, syscall.EACCES)
	}
	err = os.Chmod(tc.origFile, 0222)
	if err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}
	errCode = syscall.Access(tc.mountFile, W_OK)
	if errCode != nil {
		t.Errorf("Expected no error code for writable. %v", errCode)
	}
}

func TestMknod(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	t.Log("Testing mknod.")
	errNo := syscall.Mknod(tc.mountFile, syscall.S_IFIFO|0777, 0)
	if errNo != nil {
		t.Errorf("Mknod %v", errNo)
	}
	fi, _ := os.Lstat(tc.origFile)
	if fi == nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("Expected FIFO filetype.")
	}
}

func TestReaddir(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	contents := []byte{1, 2, 3}
	err := ioutil.WriteFile(tc.origFile, []byte(contents), 0700)
	if err != nil {
		t.Fatalf(" failed: %v", err)
	}
	err = os.Mkdir(tc.origSubdir, 0777)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	dir, err := os.Open(tc.mnt)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	infos, err := dir.Readdir(10)
	if err != nil {
		t.Fatalf("Readdir failed: %v", err)
	}

	wanted := map[string]bool{
		"hello.txt": true,
		"subdir":    true,
	}
	if len(wanted) != len(infos) {
		t.Errorf("Length mismatch %v", infos)
	} else {
		for _, v := range infos {
			_, ok := wanted[v.Name()]
			if !ok {
				t.Errorf("Unexpected name %v", v.Name())
			}
		}
	}

	dir.Close()
}

func TestFSync(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	contents := []byte{1, 2, 3}
	err := ioutil.WriteFile(tc.origFile, []byte(contents), 0700)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	f, err := os.OpenFile(tc.mountFile, os.O_WRONLY, 0)
	_, err = f.WriteString("hello there")
	if err != nil {
		t.Fatalf("WriteString failed: %v", err)
	}

	// How to really test fsync ?
	err = syscall.Fsync(int(f.Fd()))
	if err != nil {
		t.Errorf("fsync returned: %v", err)
	}
	f.Close()
}

func TestReadZero(t *testing.T) {
	ts := NewTestCase(t)
	defer ts.Cleanup()
	err := ioutil.WriteFile(ts.origFile, []byte{}, 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	back, err := ioutil.ReadFile(ts.mountFile)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if len(back) != 0 {
		t.Errorf("content length: got %d want %d", len(back), 0)
	}
}

func RandomData(size int) []byte {
	// Make blocks that are not period on 1024 bytes, so we can
	// catch errors due to misalignments.
	block := make([]byte, 1023)
	content := make([]byte, size)
	for i := range block {
		block[i] = byte(i)
	}
	start := 0
	for start < len(content) {
		left := len(content) - start
		if left < len(block) {
			block = block[:left]
		}

		copy(content[start:], block)
		start += len(block)
	}
	return content
}

func CompareSlices(t *testing.T, got, want []byte) {
	if len(got) != len(want) {
		t.Errorf("content length: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if i >= len(got) {
			break
		}
		if want[i] != got[i] {
			t.Errorf("content mismatch byte %d, got %d want %d.", i, got[i], want[i])
			break
		}
	}
}

// Check that reading large files doesn't lead to large allocations.
func TestReadLargeMemCheck(t *testing.T) {
	ts := NewTestCase(t)
	defer ts.Cleanup()

	content := RandomData(385 * 1023)
	err := ioutil.WriteFile(ts.origFile, []byte(content), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	f, err := os.Open(ts.mountFile)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	buf := make([]byte, len(content)+1024)
	f.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	f.Close()
	runtime.GC()
	var before, after runtime.MemStats

	N := 100
	runtime.ReadMemStats(&before)
	for i := 0; i < N; i++ {
		f, _ := os.Open(ts.mountFile)
		f.Read(buf)
		f.Close()
	}
	runtime.ReadMemStats(&after)
	delta := int((after.TotalAlloc - before.TotalAlloc))
	delta = (delta - 40000) / N
	t.Logf("bytes per read loop: %d", delta)
}

func TestReadLarge(t *testing.T) {
	ts := NewTestCase(t)
	defer ts.Cleanup()

	content := RandomData(385 * 1023)
	err := ioutil.WriteFile(ts.origFile, []byte(content), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	back, err := ioutil.ReadFile(ts.mountFile)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	CompareSlices(t, back, content)
}

func randomLengthString(length int) string {
	r := rand.Intn(length)
	j := 0

	b := make([]byte, r)
	for i := 0; i < r; i++ {
		j = (j + 1) % 10
		b[i] = byte(j) + byte('0')
	}
	return string(b)
}

func TestLargeDirRead(t *testing.T) {
	tc := NewTestCase(t)
	defer tc.Cleanup()

	t.Log("Testing large readdir.")
	created := 100

	names := make([]string, created)

	subdir := filepath.Join(tc.orig, "readdirSubdir")
	os.Mkdir(subdir, 0700)
	longname := "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

	nameSet := make(map[string]bool)
	for i := 0; i < created; i++ {
		// Should vary file name length.
		base := fmt.Sprintf("file%d%s", i,
			randomLengthString(len(longname)))
		name := filepath.Join(subdir, base)

		nameSet[base] = true

		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE, 0777)
		if err != nil {
			t.Fatalf("OpenFile failed: %v", err)
		}
		f.WriteString("bla")
		f.Close()

		names[i] = name
	}

	dir, err := os.Open(filepath.Join(tc.mnt, "readdirSubdir"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dir.Close()

	// Chunked read.
	total := 0
	readSet := make(map[string]bool)
	for {
		namesRead, err := dir.Readdirnames(200)
		if len(namesRead) == 0 || err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Readdirnames failed: %v", err)
		}
		for _, v := range namesRead {
			readSet[v] = true
		}
		total += len(namesRead)
	}

	if total != created {
		t.Errorf("readdir mismatch got %v wanted %v", total, created)
	}
	for k := range nameSet {
		_, ok := readSet[k]
		if !ok {
			t.Errorf("Name %v not found in output", k)
		}
	}
}

func TestRootDir(t *testing.T) {
	ts := NewTestCase(t)
	defer ts.Cleanup()

	d, err := os.Open(ts.mnt)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	_, err = d.Readdirnames(-1)
	if err != nil {
		t.Fatalf("Readdirnames failed: %v", err)
	}
	err = d.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func ioctl(fd int, cmd int, arg uintptr) (int, int) {
	r0, _, e1 := syscall.Syscall(
		syscall.SYS_IOCTL, uintptr(fd), uintptr(cmd), uintptr(arg))
	val := int(r0)
	errno := int(e1)
	return val, errno
}

func TestIoctl(t *testing.T) {
	ts := NewTestCase(t)
	defer ts.Cleanup()

	f, err := os.OpenFile(filepath.Join(ts.mnt, "hello.txt"),
		os.O_WRONLY|os.O_CREATE, 0777)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	defer f.Close()
	ioctl(int(f.Fd()), 0x5401, 42)
}

// This test is racy. If an external process consumes space while this
// runs, we may see spurious differences between the two statfs() calls.
func TestStatFs(t *testing.T) {
	ts := NewTestCase(t)
	defer ts.Cleanup()

	empty := syscall.Statfs_t{}
	s1 := empty
	err := syscall.Statfs(ts.orig, &s1)
	if err != nil {
		t.Fatal("statfs orig", err)
	}

	s2 := syscall.Statfs_t{}
	err = syscall.Statfs(ts.mnt, &s2)

	if err != nil {
		t.Fatal("statfs mnt", err)
	}

	clearStatfs(&s1)
	clearStatfs(&s2)
	if fmt.Sprintf("%v", s2) != fmt.Sprintf("%v", s1) {
		t.Errorf("statfs mismatch %#v != %#v", s1, s2)
	}
}

func TestFStatFs(t *testing.T) {
	ts := NewTestCase(t)
	defer ts.Cleanup()

	fOrig, err := os.OpenFile(ts.orig+"/file", os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	defer fOrig.Close()

	empty := syscall.Statfs_t{}
	s1 := empty
	errno := syscall.Fstatfs(int(fOrig.Fd()), &s1)
	if errno != nil {
		t.Fatal("statfs orig", err)
	}

	fMnt, err := os.OpenFile(ts.mnt+"/file", os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	defer fMnt.Close()
	s2 := empty

	errno = syscall.Fstatfs(int(fMnt.Fd()), &s2)
	if errno != nil {
		t.Fatal("statfs mnt", err)
	}

	clearStatfs(&s1)
	clearStatfs(&s2)
	if fmt.Sprintf("%v", s2) != fmt.Sprintf("%v", s1) {
		t.Errorf("statfs mismatch: %#v != %#v", s1, s2)
	}
}

func TestOriginalIsSymlink(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "go-fuse-loopback_test")
	if err != nil {
		t.Fatalf("TempDir failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	orig := tmpDir + "/orig"
	err = os.Mkdir(orig, 0755)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	link := tmpDir + "/link"
	mnt := tmpDir + "/mnt"
	err = os.Mkdir(mnt, 0755)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	err = os.Symlink("orig", link)
	if err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	fs := pathfs.NewLoopbackFileSystem(link)
	nfs := pathfs.NewPathNodeFs(fs, nil)
	state, _, err := nodefs.MountFileSystem(mnt, nfs, nil)
	if err != nil {
		t.Fatalf("MountNodeFileSystem failed: %v", err)
	}
	defer state.Unmount()

	go state.Serve()

	_, err = os.Lstat(mnt)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
}

func TestDoubleOpen(t *testing.T) {
	ts := NewTestCase(t)
	defer ts.Cleanup()

	err := ioutil.WriteFile(ts.orig+"/file", []byte("blabla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	roFile, err := os.Open(ts.mnt + "/file")
	if err != nil {
		t.Fatalf(" failed: %v", err)
	}
	defer roFile.Close()

	rwFile, err := os.OpenFile(ts.mnt+"/file", os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	defer rwFile.Close()
}

func TestUmask(t *testing.T) {
	ts := NewTestCase(t)
	defer ts.Cleanup()

	// Make sure system setting does not affect test.
	fn := ts.mnt + "/file"
	mask := 020
	cmd := exec.Command("/bin/sh", "-c",
		fmt.Sprintf("umask %o && mkdir %s", mask, fn))
	cmd.Run()

	fi, err := os.Lstat(fn)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	expect := mask ^ 0777
	got := int(fi.Mode().Perm())
	if got != expect {
		t.Errorf("got %o, expect mode %o for file %s", got, expect, fn)
	}
}
