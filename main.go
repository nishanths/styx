package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/howeyc/fsnotify"
)

// TODO(nishanths): deploy (Makefile?), -verbose, watch serve.

const versionString = "0.1.0"
const usageString = `usage:
  styx [flags] [command]`
const helpString = usageString + `

commands:
  init   initialize new site at specified path
  new    print new markdown file to stdout
  build  generate static files into "build" directory
  serve  serve "build" directory via http

flags:
  -http     http address to serve site (default: "localhost:8080")
  -watch    whether to regenerate static files on change while serving (default: false)
  -title    title of new markdown file (default: "")
  -draft    whether new markdown file is a draft (default: false)
  -verbose  verbose output (only supported for build command)`

var (
	perm = struct {
		file, dir os.FileMode
	}{0644, 0755}

	stdout = log.New(os.Stdout, "", 0)
	stderr = log.New(os.Stderr, "", 0)
)

// currentTime is set once in main and is
// used instead of time.Now() so that the same
// timestamp is used everywhere.
var currentTime time.Time

func main() {
	flags := struct {
		HTTP  string
		Watch bool
		Title string
		Draft bool

		Help    bool
		Version bool
	}{}

	flag.StringVar(&flags.HTTP, "http", "localhost:8080", "")
	flag.BoolVar(&flags.Watch, "watch", false, "")
	flag.StringVar(&flags.Title, "title", "", "")
	flag.BoolVar(&flags.Draft, "draft", false, "")
	flag.BoolVar(&flags.Help, "help", false, "")
	flag.BoolVar(&flags.Version, "version", false, "")

	flag.Usage = func() {
		stderr.Println(helpString)
		os.Exit(2)
	}
	flag.Parse()

	if flags.Help {
		stdout.Println(helpString)
		os.Exit(0)
	}
	if flags.Version {
		stdout.Println("v" + versionString)
		os.Exit(0)
	}

	command := flag.Arg(0)
	switch command {
	case "":
		stderr.Println(helpString)
		os.Exit(2)
	case "help":
		stdout.Println(helpString)
		os.Exit(0)
	case "version":
		stdout.Println("v" + versionString)
		os.Exit(0)
	}

	currentTime = time.Now()

	switch command {
	case "init":
		do(&Initialize{flag.Arg(1)})
	case "new":
		do(&New{
			Title: flags.Title,
			Draft: flags.Draft,
		})
	case "build":
		do(&Build{plugins})
	case "serve":
		do(&Serve{
			Watch: flags.Watch,
			HTTP:  flags.HTTP,
		})
	default:
		stderr.Printf("styx: unknown command %q\n", command)
		stderr.Println(`run "styx -help" for usage`)
		os.Exit(2)
	}
}

func isDir(p string) (bool, error) {
	info, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

func computeAbsDir(p string) (string, error) {
	p = filepath.Clean(p)
	if filepath.IsAbs(p) {
		return p, nil
	}

	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, p), nil
}

// do runs Cmd and exits with exit code 1 if the
// returned error is non-nil or with exit code 0 if
// the error is nil.
func do(cmd Cmd) {
	if err := cmd.Run(); err != nil {
		stderr.Println(err)
		os.Exit(1)
	}
	os.Exit(0)
}

// isEmpty returns whether a directory is empty.
func isEmpty(name string) (bool, error) {
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err // Either nil or error, suits both cases.
}

type Cmd interface {
	// Run executes the command. The error strings returned
	// should be prefixed with "styx: ".
	Run() error
}

func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return createFileWithData(dst, in)
}

type New struct {
	Title string
	Draft bool
}

func (n *New) Run() error {
	stdout.Print(FrontMatter{
		Title: n.Title,
		Draft: n.Draft,
		Time:  currentTime,
	})
	return nil
}

type Initialize struct {
	Path string // Path (absolute or relative) to initialize new site.
}

func (init *Initialize) Run() error {
	if init.Path == "" {
		return errors.New("styx: init requires path argument\nexample: styx init /path/to/new/site")
	}

	root := init.Path
	exists, err := pathExists(root)
	if err != nil {
		return err
	}
	if exists {
		empty, err := isEmpty(root)
		if err != nil {
			return err
		}
		if !empty {
			return fmt.Errorf("styx: path %q not empty", root)
		}
	}

	if err := os.MkdirAll(root, perm.dir); err != nil && !os.IsExist(err) {
		return err
	}

	success := false
	defer func() {
		// Cleanup.
		if !success {
			_ = os.RemoveAll(root) // Ignore error.
		}
	}()

	if err := os.Mkdir(filepath.Join(root, "src"), perm.dir); err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	errs := make(chan error, len(rawFiles))
	for k, v := range rawFiles {
		k, v := k, v
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- createFileWithData(
				filepath.Join(root, filepath.FromSlash(k)),
				bytes.NewReader(v),
			)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}

	success = true
	return nil
}

type Serve struct {
	HTTP  string
	Watch bool
}

func (s *Serve) Run() error {
	if s.Watch {
		w, err := fsnotify.NewWatcher()
		if err != nil {
			return err
		}
		defer w.Close()
		stderr.Println(`watching "src/*/**" for changes ...`)

		if err := filepath.Walk("src", func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return nil
			}
			go func() {
				go func() {
					for err := range w.Error {
						stderr.Println("error: watch:", err)
					}
				}()
				go func() {
					for e := range w.Event {
						stderr.Printf("rebuilding change: %q ... ", e.Name)
						if err := (&Build{plugins}).Run(); err != nil {
							stderr.Println("error: rebuild:", err)
						} else {
							stderr.Printf("done rebuilding")
						}
					}
				}()
				if err := w.Watch(p); err != nil {
					stderr.Println("error: watch:", err)
				}
			}()
			return nil
		}); err != nil {
			return err
		}
	}

	stderr.Printf("serving http on %s ...\n", s.HTTP)
	return http.ListenAndServe(s.HTTP, http.FileServer(http.Dir("build")))
}

func pathExists(p string) (bool, error) {
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

// createFile creates a file with the supplied name.
// If the error is non-nil, the caller is responsible for calling Close.
func createFile(name string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(name), perm.dir); err != nil {
		return nil, err
	}
	return os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm.file)
}

// createFileWithData creates and writes a file with the supplied data.
func createFileWithData(name string, data io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(name), perm.dir); err != nil {
		return err
	}
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm.file)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = io.Copy(f, data); err != nil {
		return err
	}
	return f.Sync()
}
