// Package thumb implements a lazy image thumbnail cache. Supported input image
// formats are any format Go can decode natively from the standard library and
// subrepo golang.org/x/image.
package thumb

import (
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/nfnt/resize"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
	"golang.org/x/image/webp"
)

const ThumbSize = 100

// Encoder describes a way to encode a thumbnail image.
type Encoder interface {
	Extension() string // The file extension of the resulting image
	Encode(dst io.Writer, thumb image.Image) error
}

type JPEGEncoder struct{ *jpeg.Options }

func (JPEGEncoder) Extension() string { return ".jpg" }
func (e JPEGEncoder) Encode(dst io.Writer, thumb image.Image) error {
	return jpeg.Encode(dst, thumb, e.Options)
}

// FileStore is a source of files that Cache will reference.
type FileStore interface {
	// Get should return the path to the file on disk, or the empty string if
	// not found.
	Get(id string) string
}

// Cache is a lazy, concurrent thumbnail cache for airlift-server with request
// batching for on-the-fly thumbnail generation. Only file paths are cached in
// memory.
type Cache struct {
	size     int64  // the total size of the thumbnails
	dir      string // path of directory where thumbnails are stored
	enc      Encoder
	store    FileStore
	files    map[string]struct{}
	req      chan string      // ID
	remove   chan string      // send ID, or empty string to purge all
	resp     chan interface{} // file path
	inflight map[string][]chan string
}

func NewCache(dirPath string, enc Encoder, store FileStore) (*Cache, error) {
	c := &Cache{
		dir:      dirPath,
		enc:      enc,
		store:    store,
		files:    make(map[string]struct{}),
		req:      make(chan string),
		remove:   make(chan string),
		resp:     make(chan interface{}),
		inflight: make(map[string][]chan string),
	}

	os.MkdirAll(dirPath, 0755)
	dir, err := os.Open(dirPath)
	if err != nil {
		return nil, err
	}
	fis, err := dir.Readdir(0)
	if err != nil {
		return nil, err
	}

	for _, fi := range fis {
		c.size += fi.Size()
		name := fi.Name()
		id := name[:len(name)-len(filepath.Ext(name))]
		c.files[id] = struct{}{}
	}

	return c, nil
}

// Listen starts the cache request server, blocking forever. It should be
// launched in its own goroutine before any requests are made.
func (c *Cache) Serve() {
	for {
		select {
		case id := <-c.req:
			if _, ok := c.files[id]; ok {
				ch := make(chan string)
				c.resp <- ch
				ch <- c.thumbPath(id)
			} else {
				c.getThumb(id)
			}
		case id := <-c.remove:
			if id == "" {
				c.resp <- c.doPurge()
			} else {
				c.resp <- c.doRemove(id)
			}
		}
	}
}

func (c *Cache) Size() int64 {
	return c.size
}

func (c *Cache) thumbPath(id string) string {
	return filepath.Join(c.dir, id) + c.enc.Extension()
}

// Get the file path to the thumbnail of the file with the given id. Generate
// it if it doesn't exist already. If concurrent requests are made to the same
// non-existent thumbnail, it will only be generated once.
func (c *Cache) Get(id string) string {
	c.req <- id
	resp := (<-c.resp).(chan string)
	return <-resp
}

func (c *Cache) getThumb(id string) {
	ch := make(chan string, 1)
	c.resp <- ch
	c.inflight[id] = append(c.inflight[id], ch)
	// if there is a request happening on this already, simply add a reciever
	// to the list and let them wait for it
	if len(c.inflight[id]) > 1 {
		return
	}

	go func() {
		// now we enter the part of the function that actually does the work
		path := new(string)

		// once the work is done, send to all the recievers
		defer func() {
			for _, ch := range c.inflight[id] {
				ch <- *path
			}
			delete(c.inflight, id)
		}()

		src := c.store.Get(id)
		decoder := DecodeFunc(src)
		if decoder == nil {
			return
		}

		// generate thumb

		f, err := os.Open(src)
		if err != nil {
			log.Print("getThumb: ", err)
			return
		}

		p := c.thumbPath(id)
		dst, err := os.Create(p)
		if err != nil {
			log.Print("getThumb: ", err)
			return
		}

		img, err := decoder(f)
		if err != nil {
			log.Print("getThumb: ", err)
			return
		}
		thumb := resize.Thumbnail(ThumbSize, ThumbSize, img, resize.Bilinear)
		if err := c.enc.Encode(dst, thumb); err != nil {
			os.Remove(p)
			log.Print("getThumb: ", err)
			return
		}

		fi, err := dst.Stat()
		if err != nil {
			os.Remove(p)
			log.Print("getThumb: ", err)
			return
		}

		c.size += fi.Size()
		c.files[id] = struct{}{}
		*path = p
	}()
}

func (c *Cache) Purge() error {
	c.remove <- ""
	return (<-c.resp).(error)
}

func (c *Cache) doPurge() error {
	for id := range c.files {
		if err := c.doRemove(id); err != nil {
			return err
		}
	}

	return nil
}

func (c *Cache) Remove(id string) error {
	c.remove <- id
	return (<-c.resp).(error)
}

func (c *Cache) doRemove(id string) error {
	path := c.thumbPath(id)
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	size := fi.Size()
	if err = os.Remove(path); err != nil {
		return err
	}
	c.size -= size
	delete(c.files, id)

	return nil
}

var decodeFuncMap = map[string]func(io.Reader) (image.Image, error){
	".jpg":  jpeg.Decode,
	".jpeg": jpeg.Decode,
	".png":  png.Decode,
	".gif":  gif.Decode,
	".tif":  tiff.Decode,
	".tiff": tiff.Decode,
	".webp": webp.Decode,
	".bmp":  bmp.Decode,
}

// DecodeFunc returns a func that can be used to decode the image with the
// given file name, or nil if it's not supported.
// TODO: sniff magic number instead of only using file extension
// TODO: allow externally registered format decoders
func DecodeFunc(name string) func(io.Reader) (image.Image, error) {
	ext := strings.ToLower(filepath.Ext(name))
	return decodeFuncMap[ext]
}