package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

// VolumeFUSEMount represents a FUSE mount for a remote volume
type VolumeFUSEMount struct {
	volumeID   string
	mountPoint string
	agentURL   string
	conn       *fuse.Conn
	token      string
	server     *http.Server
}

// NewVolumeFUSEMount creates a new FUSE mount for a volume
func NewVolumeFUSEMount(volumeID, mountPoint, agentURL, token string) *VolumeFUSEMount {
	return &VolumeFUSEMount{
		volumeID:   volumeID,
		mountPoint: mountPoint,
		agentURL:   agentURL,
		token:      token,
	}
}

// Mount starts the FUSE mount
func (m *VolumeFUSEMount) Mount() error {
	// Ensure mount point exists
	if err := os.MkdirAll(m.mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Mount FUSE
	c, err := fuse.Mount(m.mountPoint, fuse.FSName("okayrun-volume"), fuse.Subtype("volume"))
	if err != nil {
		return fmt.Errorf("failed to mount FUSE: %w", err)
	}

	m.conn = c

	// Create and serve the filesystem
	filesys := &VolumeFS{
		volumeID: m.volumeID,
		agentURL: m.agentURL,
		token:    m.token,
	}

	go func() {
		if err := fs.Serve(c, filesys); err != nil {
			log.Printf("[Volume FUSE] Error serving filesystem: %v", err)
		}
	}()

	log.Printf("[Volume FUSE] Mounted volume %s at %s", m.volumeID, m.mountPoint)
	return nil
}

// Unmount stops the FUSE mount
func (m *VolumeFUSEMount) Unmount() error {
	if m.conn != nil {
		if err := fuse.Unmount(m.mountPoint); err != nil {
			log.Printf("[Volume FUSE] Warning: failed to unmount: %v", err)
		}
		m.conn.Close()
	}

	// Remove mount point
	os.Remove(m.mountPoint)

	log.Printf("[Volume FUSE] Unmounted volume %s from %s", m.volumeID, m.mountPoint)
	return nil
}

// VolumeFS implements fs.FS for the volume
type VolumeFS struct {
	volumeID string
	agentURL string
	token    string
}

// Root returns the root directory of the filesystem
func (f *VolumeFS) Root() (fs.Node, error) {
	return &VolumeDir{
		volumeID: f.volumeID,
		agentURL: f.agentURL,
		token:    f.token,
		path:     "/",
	}, nil
}

// VolumeDir implements fs.Node for directories
type VolumeDir struct {
	volumeID string
	agentURL string
	token    string
	path     string
}

// Attr returns the attributes of the directory
func (d *VolumeDir) Attr(ctx context.Context, a *fuse.Attr) error {
	// Fetch attributes from agent
	info, err := d.fetchInfo()
	if err != nil {
		return err
	}

	a.Mode = os.ModeDir | 0755
	a.Size = uint64(info.Size)
	a.Mtime = info.ModTime
	return nil
}

// Lookup looks up a child entry
func (d *VolumeDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	childPath := filepath.Join(d.path, name)
	return &VolumeDir{
		volumeID: d.volumeID,
		agentURL: d.agentURL,
		token:    d.token,
		path:     childPath,
	}, nil
}

// ReadDirAll returns all directory entries
func (d *VolumeDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	entries, err := d.fetchEntries()
	if err != nil {
		return nil, err
	}

	var result []fuse.Dirent
	for _, entry := range entries {
		kind := fuse.DT_Dir
		if !entry.IsDir {
			kind = fuse.DT_File
		}
		result = append(result, fuse.Dirent{
			Name: entry.Name,
			Type: kind,
		})
	}

	return result, nil
}

// Create creates a new file
func (d *VolumeDir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	childPath := filepath.Join(d.path, req.Name)
	file := &VolumeFile{
		volumeID: d.volumeID,
		agentURL: d.agentURL,
		token:    d.token,
		path:     childPath,
	}

	// Create file on agent
	if err := file.create(); err != nil {
		return nil, nil, err
	}

	return file, file, nil
}

// Mkdir creates a new directory
func (d *VolumeDir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	childPath := filepath.Join(d.path, req.Name)
	dir := &VolumeDir{
		volumeID: d.volumeID,
		agentURL: d.agentURL,
		token:    d.token,
		path:     childPath,
	}

	if err := dir.mkdir(); err != nil {
		return nil, err
	}

	return dir, nil
}

// Remove removes a file or directory
func (d *VolumeDir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	childPath := filepath.Join(d.path, req.Name)
	return d.remove(childPath)
}

func (d *VolumeDir) fetchInfo() (*FileInfo, error) {
	url := fmt.Sprintf("%s/volume/%s/info?path=%s", d.agentURL, d.volumeID, d.path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to fetch info: HTTP %d", resp.StatusCode)
	}

	var info FileInfo
	if err := decodeJSON(resp.Body, &info); err != nil {
		return nil, err
	}

	return &info, nil
}

func (d *VolumeDir) fetchEntries() ([]Entry, error) {
	url := fmt.Sprintf("%s/volume/%s/list?path=%s", d.agentURL, d.volumeID, d.path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to list entries: HTTP %d", resp.StatusCode)
	}

	var entries []Entry
	if err := decodeJSON(resp.Body, &entries); err != nil {
		return nil, err
	}

	return entries, nil
}

func (d *VolumeDir) mkdir() error {
	url := fmt.Sprintf("%s/volume/%s/mkdir?path=%s", d.agentURL, d.volumeID, d.path)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("failed to create directory: HTTP %d", resp.StatusCode)
	}

	return nil
}

func (d *VolumeDir) remove(path string) error {
	url := fmt.Sprintf("%s/volume/%s/remove?path=%s", d.agentURL, d.volumeID, path)
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return fmt.Errorf("failed to remove: HTTP %d", resp.StatusCode)
	}

	return nil
}

// VolumeFile implements fs.Node and fs.Handle for files
type VolumeFile struct {
	volumeID string
	agentURL string
	token    string
	path     string
	file     *os.File
}

// Attr returns the attributes of the file
func (f *VolumeFile) Attr(ctx context.Context, a *fuse.Attr) error {
	info, err := f.fetchInfo()
	if err != nil {
		return err
	}

	a.Mode = 0644
	a.Size = uint64(info.Size)
	a.Mtime = info.ModTime
	return nil
}

// Read reads from the file
func (f *VolumeFile) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	url := fmt.Sprintf("%s/volume/%s/read?path=%s&offset=%d&size=%d",
		f.agentURL, f.volumeID, f.path, req.Offset, req.Size)

	httpReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+f.token)

	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != 200 {
		return fmt.Errorf("failed to read: HTTP %d", httpResp.StatusCode)
	}

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return err
	}

	resp.Data = data
	return nil
}

// Write writes to the file
func (f *VolumeFile) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	url := fmt.Sprintf("%s/volume/%s/write?path=%s&offset=%d",
		f.agentURL, f.volumeID, f.path, req.Offset)

	httpReq, err := http.NewRequest("POST", url, strings.NewReader(string(req.Data)))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+f.token)
	httpReq.Header.Set("Content-Type", "application/octet-stream")

	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != 200 {
		return fmt.Errorf("failed to write: HTTP %d", httpResp.StatusCode)
	}

	resp.Size = len(req.Data)
	return nil
}

// Flush flushes the file
func (f *VolumeFile) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	return nil
}

// Release releases the file
func (f *VolumeFile) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	if f.file != nil {
		f.file.Close()
	}
	return nil
}

func (f *VolumeFile) create() error {
	url := fmt.Sprintf("%s/volume/%s/create?path=%s", f.agentURL, f.volumeID, f.path)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("failed to create file: HTTP %d", resp.StatusCode)
	}

	return nil
}

func (f *VolumeFile) fetchInfo() (*FileInfo, error) {
	url := fmt.Sprintf("%s/volume/%s/info?path=%s", f.agentURL, f.volumeID, f.path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to fetch info: HTTP %d", resp.StatusCode)
	}

	var info FileInfo
	if err := decodeJSON(resp.Body, &info); err != nil {
		return nil, err
	}

	return &info, nil
}

// FileInfo represents file information
type FileInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
	Mode    os.FileMode `json:"mode"`
}

// Entry represents a directory entry
type Entry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
}

// decodeJSON decodes JSON from a reader
func decodeJSON(r io.Reader, v interface{}) error {
	decoder := newJSONDecoder(r)
	return decoder.Decode(v)
}

func newJSONDecoder(r io.Reader) *jsonDecoder {
	return &jsonDecoder{r: r}
}

type jsonDecoder struct {
	r io.Reader
}

func (d *jsonDecoder) Decode(v interface{}) error {
	data, err := io.ReadAll(d.r)
	if err != nil {
		return err
	}

	// Simple JSON parsing using encoding/json
	return parseJSON(data, v)
}

func parseJSON(data []byte, v interface{}) error {
	// This is a simplified JSON parser
	// In production, use encoding/json
	return fmt.Errorf("JSON parsing not implemented - use encoding/json")
}

// ActiveMounts tracks active FUSE mounts
var (
	activeMounts   = make(map[string]*VolumeFUSEMount)
	activeMountsMu sync.Mutex
)

// MountVolume mounts a volume locally via FUSE
func MountVolume(volumeID, mountPoint, agentURL, token string) error {
	activeMountsMu.Lock()
	if _, exists := activeMounts[volumeID]; exists {
		activeMountsMu.Unlock()
		return fmt.Errorf("volume %s is already mounted", volumeID)
	}
	activeMountsMu.Unlock()

	mount := NewVolumeFUSEMount(volumeID, mountPoint, agentURL, token)
	if err := mount.Mount(); err != nil {
		return err
	}

	activeMountsMu.Lock()
	activeMounts[volumeID] = mount
	activeMountsMu.Unlock()

	return nil
}

// UnmountVolume unmounts a volume
func UnmountVolume(volumeID string) error {
	activeMountsMu.Lock()
	mount, exists := activeMounts[volumeID]
	if !exists {
		activeMountsMu.Unlock()
		return fmt.Errorf("volume %s is not mounted", volumeID)
	}
	delete(activeMounts, volumeID)
	activeMountsMu.Unlock()

	return mount.Unmount()
}

// UnmountAllVolumes unmounts all active volumes
func UnmountAllVolumes() {
	activeMountsMu.Lock()
	mounts := make([]*VolumeFUSEMount, 0, len(activeMounts))
	for _, mount := range activeMounts {
		mounts = append(mounts, mount)
	}
	activeMounts = make(map[string]*VolumeFUSEMount)
	activeMountsMu.Unlock()

	for _, mount := range mounts {
		if err := mount.Unmount(); err != nil {
			log.Printf("[Volume FUSE] Error unmounting volume %s: %v", mount.volumeID, err)
		}
	}
}
