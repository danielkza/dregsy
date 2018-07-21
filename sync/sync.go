/*
 *
 */

package sync

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/client"
	"golang.org/x/crypto/ssh/terminal"

	"github.com/xelalexv/dregsy/docker"
)

//
var toTerminal bool

func init() {
	toTerminal = terminal.IsTerminal(int(os.Stdout.Fd()))
}

//
type Sync struct {
	client *docker.Client
}

//
func New(dockerhost, api string) (*Sync, error) {

	sync := &Sync{}

	var out io.Writer = sync
	if toTerminal {
		out = nil
	}

	if dockerhost == "" {
		dockerhost = client.DefaultDockerHost
	}

	if api == "" {
		api = "1.24"
	}

	cli, err := docker.NewClient(dockerhost, api, out)
	if err != nil {
		return nil, fmt.Errorf("cannot create Docker client: %v", err)
	}

	sync.client = cli
	return sync, nil
}

//
func (s *Sync) Dispose() {
	s.client.Close()
}

//
func (s *Sync) SyncFromConfig(conf *syncConfig) error {

	// when we begin, Docker daemon may not be ready yet, e.g. when dregsy runs
	// side by side with a Docker-in-Docker container inside a pod on k8s
	LogPrintln()
	LogInfo("pinging Docker daemon...")

	if _, err := s.client.Ping(30, 10*time.Second); err != nil {
		LogError(err)
	} else {
		LogInfo("ok")
	}

	// one-off tasks
	for _, t := range conf.Tasks {
		if t.Interval == 0 {
			s.SyncTask(t)
		}
	}

	// periodic tasks
	c := make(chan *task)
	ticking := false

	for _, t := range conf.Tasks {
		if t.Interval > 0 {
			t.startTicking(c)
			ticking = true
		}
	}

	for ticking {
		LogPrintln()
		LogInfo("waiting for next sync task...")
		LogPrintln()
		s.SyncTask(<-c)
	}

	LogInfo("all done")
	return nil
}

//
func (s *Sync) SyncTask(t *task) {

	LogInfo("syncing task '%s': '%s' --> '%s'",
		t.Name, t.Source.Registry, t.Target.Registry)

	for _, m := range t.Mappings {
		LogInfo("mapping '%s' to '%s'", m.From, m.To)
		src, trgt := t.mappingRefs(m)
		LogError(t.Source.refreshAuth())
		LogError(t.Target.refreshAuth())
		LogError(t.ensureTargetExists(trgt))
		LogError(s.Sync(
			src, t.Source.Auth, trgt, t.Target.Auth, m.Tags, t.Verbose))
	}

	LogPrintln()
}

//
func (s *Sync) Sync(srcRef, srcAuth, trgtRef, trgtAuth string, tags []string,
	verbose bool) error {

	LogInfo("pulling source image '%s'", srcRef)
	var err error

	if len(tags) == 0 {
		if err = s.pull(srcRef, srcAuth, true, verbose); err != nil {
			return fmt.Errorf(
				"error pulling source image '%s': %v", srcRef, err)
		}

	} else {
		for _, tag := range tags {
			srcRefTagged := fmt.Sprintf("%s:%s", srcRef, tag)
			if err = s.pull(srcRefTagged, srcAuth, false, verbose); err != nil {
				return fmt.Errorf(
					"error pulling source image '%s': %v", srcRefTagged, err)
			}
		}
	}

	LogInfo("relevant tags")
	var srcImages []*docker.Image

	if len(tags) == 0 {
		srcImages, err = s.list(srcRef)
		if err != nil {
			LogError(
				fmt.Errorf("error listing all tags of source image '%s': %v",
					srcRef, err))
		}

	} else {
		for _, tag := range tags {
			srcRefTagged := fmt.Sprintf("%s:%s", srcRef, tag)
			srcImageTagged, err := s.list(srcRefTagged)
			if err != nil {
				LogError(
					fmt.Errorf("error listing source image '%s': %v",
						srcRefTagged, err))
			}
			srcImages = append(srcImages, srcImageTagged...)
		}
	}

	for _, img := range srcImages {
		LogInfo(" - %s", img.RefWithTags())
	}

	LogInfo("setting tags for target image '%s'", trgtRef)

	_, err = s.tag(srcImages, trgtRef)
	if err != nil {
		return fmt.Errorf("error setting tags: %v", err)
	}

	LogInfo("pushing target image '%s'", trgtRef)

	if err := s.push(trgtRef, trgtAuth, verbose); err != nil {
		return fmt.Errorf("error pushing target image: %v", err)
	}

	return nil
}

//
func (s *Sync) pull(ref, auth string, allTags, verbose bool) error {
	return s.client.PullImage(ref, allTags, auth, verbose)
}

//
func (s *Sync) list(ref string) ([]*docker.Image, error) {
	return s.client.ListImages(ref)
}

//
func (s *Sync) tag(images []*docker.Image, targetRef string) ([]*docker.Image,
	error) {

	taggedImages := []*docker.Image{}
	targetRepo, targetPath, _ := docker.SplitRef(targetRef)

	for _, img := range images {
		tagged := &docker.Image{
			ID:   img.ID,
			Repo: targetRepo,
			Path: targetPath,
			Tags: img.Tags,
		}
		for _, tag := range img.Tags {
			if err := s.client.TagImage(img.ID, fmt.Sprintf("%s:%s",
				tagged.Ref(), tag)); err != nil {
				return nil, err
			}
		}
		taggedImages = append(taggedImages, tagged)
	}

	return taggedImages, nil
}

//
func (s *Sync) push(ref, auth string, verbose bool) error {
	return s.client.PushImage(ref, true, auth, verbose)
}

// -----------------------------------------------------------------------------

//
func (s *Sync) Write(p []byte) (n int, err error) {
	fmt.Print(string(p))
	return len(p), nil
}

//
func LogPrintln() {
	LogInfo("")
}

//
func LogWarning(msg string, params ...interface{}) {
	log("WARN", msg, params...)
}

//
func LogInfo(msg string, params ...interface{}) {
	log("INFO", msg, params...)
}

//
func log(level, msg string, params ...interface{}) {
	msg = fmt.Sprintf(msg, params...)
	if !toTerminal {
		msg = fmt.Sprintf(
			"%s [%s] %s", time.Now().Format(time.RFC3339), level, msg)
	}
	fmt.Print(msg)
	if !strings.HasSuffix(msg, "\n") {
		fmt.Println()
	}
}

//
func LogError(err error) bool {
	if err != nil {
		if toTerminal {
			fmt.Fprintf(os.Stderr, "%v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "%s [ERROR] %v\n",
				time.Now().Format(time.RFC3339), err)
		}
		return true
	}
	return false
}