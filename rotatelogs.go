// package rotatelogs is a port of File-RotateLogs from Perl
// (https://metacpan.org/release/File-RotateLogs), and it allows
// you to automatically rotate output files when you write to them
// according to the filename pattern that you can specify.
package rotatelogs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/etsme-com/file-rotatelogs/internal/fileutil"
	strftime "github.com/lestrrat-go/strftime"
	"github.com/pkg/errors"
)

func (c clockFn) Now() time.Time {
	return c()
}

// New creates a new RotateLogs object. A log filename pattern
// must be passed. Optional `Option` parameters may be passed
func New(p string, options ...Option) (*RotateLogs, error) {
	globPattern := p
	for _, re := range patternConversionRegexps {
		globPattern = re.ReplaceAllString(globPattern, "*")
	}

	pattern, err := strftime.New(p)
	if err != nil {
		return nil, errors.Wrap(err, `invalid strftime pattern`)
	}

	var clock Clock = Local
	rotationTime := 24 * time.Hour
	var rotationSize int64
	var rotationCount uint
	var linkName string
	var maxAge time.Duration
	var handler Handler
	var forceNewFile bool

	for _, o := range options {
		switch o.Name() {
		case optkeyClock:
			clock = o.Value().(Clock)
		case optkeyLinkName:
			linkName = o.Value().(string)
		case optkeyMaxAge:
			maxAge = o.Value().(time.Duration)
			if maxAge < 0 {
				maxAge = 0
			}
		case optkeyRotationTime:
			rotationTime = o.Value().(time.Duration)
			if rotationTime < 0 {
				rotationTime = 0
			}
		case optkeyRotationSize:
			rotationSize = o.Value().(int64)
			if rotationSize < 0 {
				rotationSize = 0
			}
		case optkeyRotationCount:
			rotationCount = o.Value().(uint)
		case optkeyHandler:
			handler = o.Value().(Handler)
		case optkeyForceNewFile:
			forceNewFile = true
		}
	}

	if maxAge > 0 && rotationCount > 0 {
		return nil, errors.New("options MaxAge and RotationCount cannot be both set")
	}

	if maxAge == 0 && rotationCount == 0 {
		// if both are 0, give maxAge a sane default
		maxAge = 7 * 24 * time.Hour
	}

	return &RotateLogs{
		clock:         clock,
		eventHandler:  handler,
		globPattern:   globPattern,
		linkName:      linkName,
		maxAge:        maxAge,
		pattern:       pattern,
		rotationTime:  rotationTime,
		rotationSize:  rotationSize,
		rotationCount: rotationCount,
		forceNewFile:  forceNewFile,
	}, nil
}

// Write satisfies the io.Writer interface. It writes to the
// appropriate file handle that is currently being used.
// If we have reached rotation time, the target file gets
// automatically rotated, and also purged if necessary.
func (rl *RotateLogs) Write(p []byte) (n int, err error) {
	// Guard against concurrent writes
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	out, err := rl.getWriterNolock(false, false)
	if err != nil {
		return 0, errors.Wrap(err, `failed to acquite target io.Writer`)
	}

	return out.Write(p)
}

// must be locked during this operation
func (rl *RotateLogs) getWriterNolock(bailOnRotateFail, useGenerationalNames bool) (io.Writer, error) {
	generation := rl.generation
	previousFn := rl.curFn

	// This filename contains the name of the "NEW" filename
	// to log to, which may be newer than rl.currentFilename
	baseFn := fileutil.GenerateFn(rl.pattern, rl.clock, rl.rotationTime)
	filename := baseFn
	var forceNewFile bool

	fi, err := os.Stat(rl.curFn)
	sizeRotation := false
	if err == nil && rl.rotationSize > 0 && rl.rotationSize <= fi.Size() {
		forceNewFile = true
		sizeRotation = true
	}

	//fmt.Println("generation = ", generation, baseFn)
	//fmt.Println("previousFn = ", previousFn)
	//fmt.Println("rl.curBaseFn = ", rl.curBaseFn)

	if baseFn != rl.curBaseFn {
		// First time, continue to use the newest file. Modified by chenyan
		if rl.curBaseFn == "" {
			matches, err := filepath.Glob(rl.globPattern)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to glob directory %s", rl.globPattern)
			}

			//fmt.Println("===len(matches) = ", len(matches))

			var maxFileTime int64 = 0

			for _, path := range matches {
				//fmt.Println("===path = ", path)

				if strings.Contains(path, "rotation") == false {
					generation = 0
					//fmt.Println("===path = ", path)
					continue
				}

				fi, err := os.Stat(path)
				if err != nil {
					continue
				}

				mtime := fi.ModTime().Unix()
				//fmt.Println("===fi = ", path, fi.ModTime(), mtime, maxFileTime)

				if mtime >= maxFileTime {
					maxFileTime = mtime

					idx := strings.LastIndex(path, ".")

					generationStr := path[idx+1:]
					generation, _ = strconv.Atoi(generationStr)
					//fmt.Println(generation)
				}
			}
			////fmt.Println("===filename = ", filename, len(matches))
		} else {
			generation = 0
		}

		forceNewFile = true
		//fmt.Println("generation =", generation)

		// even though this is the first write after calling New(),
		// check if a new file needs to be created
		//if rl.forceNewFile {
		//	forceNewFile = true
		//}
	} else {
		if !useGenerationalNames && !sizeRotation {
			// nothing to do
			//fmt.Println("nothing to do")
			return rl.outFh, nil
		}
		forceNewFile = true
		generation++
	}
	if forceNewFile {
		// A new file has been requested. Instead of just using the
		// regular strftime pattern, we create a new file name using
		// generational names such as "foo.1", "foo.2", "foo.3", etc
		var name string
		for {
			if generation == 0 {
				name = filename
				//fmt.Println("generation == 0")
			} else {
				name = fmt.Sprintf("%s.rotation.%d", filename, generation)
				//fmt.Println("generation > 0")
			}

			// Only Create new file when the size of file is bigger than r1.rotationSize. Modified by chenyan
			if tmpfi, err := os.Stat(name); err != nil || tmpfi.Size() < rl.rotationSize {
				filename = name
				//fmt.Println("---filename =", filename)
				break
			}
			generation++
			//fmt.Println("generation++ =", generation)
		}
	}

	fh, err := fileutil.CreateFile(filename)
	if err != nil {
		return nil, errors.Wrapf(err, `failed to create a new file %v`, filename)
	}

	if err := rl.rotateNolock(filename); err != nil {
		err = errors.Wrap(err, "failed to rotate")
		if bailOnRotateFail {
			// Failure to rotate is a problem, but it's really not a great
			// idea to stop your application just because you couldn't rename
			// your log.
			//
			// We only return this error when explicitly needed (as specified by bailOnRotateFail)
			//
			// However, we *NEED* to close `fh` here
			if fh != nil { // probably can't happen, but being paranoid
				fh.Close()
			}

			return nil, err
		}
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
	}

	rl.outFh.Close()
	rl.outFh = fh
	rl.curBaseFn = baseFn
	rl.curFn = filename
	rl.generation = generation

	if h := rl.eventHandler; h != nil {
		go h.Handle(&FileRotatedEvent{
			prev:    previousFn,
			current: filename,
		})
	}

	return fh, nil
}

// CurrentFileName returns the current file name that
// the RotateLogs object is writing to
func (rl *RotateLogs) CurrentFileName() string {
	rl.mutex.RLock()
	defer rl.mutex.RUnlock()

	return rl.curFn
}

var patternConversionRegexps = []*regexp.Regexp{
	regexp.MustCompile(`%[%+A-Za-z]`),
	regexp.MustCompile(`\*+`),
}

type cleanupGuard struct {
	enable bool
	fn     func()
	mutex  sync.Mutex
}

func (g *cleanupGuard) Enable() {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	g.enable = true
}

func (g *cleanupGuard) Run() {
	g.fn()
}

// Rotate forcefully rotates the log files. If the generated file name
// clash because file already exists, a numeric suffix of the form
// ".1", ".2", ".3" and so forth are appended to the end of the log file
//
// Thie method can be used in conjunction with a signal handler so to
// emulate servers that generate new log files when they receive a
// SIGHUP
func (rl *RotateLogs) Rotate() error {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()
	_, err := rl.getWriterNolock(true, true)

	return err
}

func (rl *RotateLogs) rotateNolock(filename string) error {
	lockfn := filename + `_lock`
	fh, err := os.OpenFile(lockfn, os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		// Can't lock, just return
		return err
	}

	var guard cleanupGuard
	guard.fn = func() {
		fh.Close()
		os.Remove(lockfn)
	}
	defer guard.Run()

	if rl.linkName != "" {
		tmpLinkName := filename + `_symlink`

		// Change how the link name is generated based on where the
		// target location is. if the location is directly underneath
		// the main filename's parent directory, then we create a
		// symlink with a relative path
		linkDest := filename
		linkDir := filepath.Dir(rl.linkName)

		baseDir := filepath.Dir(filename)
		if strings.Contains(rl.linkName, baseDir) {
			tmp, err := filepath.Rel(linkDir, filename)
			if err != nil {
				return errors.Wrapf(err, `failed to evaluate relative path from %#v to %#v`, baseDir, rl.linkName)
			}

			linkDest = tmp
		}

		if err := os.Symlink(linkDest, tmpLinkName); err != nil {
			return errors.Wrap(err, `failed to create new symlink`)
		}

		// the directory where rl.linkName should be created must exist
		_, err := os.Stat(linkDir)
		if err != nil { // Assume err != nil means the directory doesn't exist
			if err := os.MkdirAll(linkDir, 0755); err != nil {
				return errors.Wrapf(err, `failed to create directory %s`, linkDir)
			}
		}

		if err := os.Rename(tmpLinkName, rl.linkName); err != nil {
			return errors.Wrap(err, `failed to rename new symlink`)
		}
	}

	if rl.maxAge <= 0 && rl.rotationCount <= 0 {
		return errors.New("panic: maxAge and rotationCount are both set")
	}

	matches, err := filepath.Glob(rl.globPattern)
	if err != nil {
		return err
	}

	cutoff := rl.clock.Now().Add(-1 * rl.maxAge)

	// the linter tells me to pre allocate this...
	toUnlink := make([]string, 0, len(matches))
	for _, path := range matches {
		// Ignore lock files
		if strings.HasSuffix(path, "_lock") || strings.HasSuffix(path, "_symlink") {
			continue
		}

		fi, err := os.Stat(path)
		if err != nil {
			continue
		}

		fl, err := os.Lstat(path)
		if err != nil {
			continue
		}

		if rl.maxAge > 0 && fi.ModTime().After(cutoff) {
			continue
		}

		if rl.rotationCount > 0 && fl.Mode()&os.ModeSymlink == os.ModeSymlink {
			continue
		}
		toUnlink = append(toUnlink, path)
	}

	var toUnlinkfile string

	if rl.rotationCount > 0 {
		// Only delete if we have more than rotationCount
		if rl.rotationCount >= uint(len(toUnlink)) {
			return nil
		}

		// Delete the oldest file. Modified by chenyan
		minFileTime := time.Now().Unix()
		//fmt.Println("----minFileTime = ", minFileTime)

		for _, path := range toUnlink {
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}

			mtime := fi.ModTime().Unix()
			//fmt.Println("===fi = ", path, fi.ModTime(), mtime)

			if mtime < minFileTime {
				minFileTime = mtime
				toUnlinkfile = path
				//fmt.Println("toUnlinkfile = ", toUnlinkfile, minFileTime)
			}

			//fmt.Println("minFileTime = ", minFileTime)
		}

		//toUnlink = toUnlink[:len(toUnlink)-int(rl.rotationCount)]
		////fmt.Println("===toUnlink = ", toUnlink)
	}

	//fmt.Println("toUnlinkfile = ", toUnlinkfile)

	//if len(toUnlink) <= 0 {
	//	return nil
	//}

	guard.Enable()
	go func() {
		// unlink files on a separate goroutine
		//for _, path := range toUnlink {
		os.Remove(toUnlinkfile)
		//}
	}()

	return nil
}

// Close satisfies the io.Closer interface. You must
// call this method if you performed any writes to
// the object.
func (rl *RotateLogs) Close() error {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	if rl.outFh == nil {
		return nil
	}

	rl.outFh.Close()
	rl.outFh = nil

	return nil
}
