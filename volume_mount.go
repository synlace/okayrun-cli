package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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
	baseURL    string
	username   string
	password   string
	conn       *fuse.Conn
}

// NewVolumeFUSEMount creates a new FUSE mount for a volume
func NewVolumeFUSEMount(volumeID, mountPoint, baseURL, username, password string) *VolumeFUSEMount {
	return &VolumeFUSEMount{
		volumeID:   volumeID,
		mountPoint: mountPoint,
		baseURL:    strings.TrimRight(baseURL, "/"),
		username:   username,
		password:   password,
	}
}

// Mount starts the FUSE mount
func (m *VolumeFUSEMount) Mount() error {
	if err := os.MkdirAll(m.mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	c, err := fuse.Mount(m.mountPoint, fuse.FSName("okayrun-volume"), fuse.Subtype("volume"))
	if err != nil {
		return fmt.Errorf("failed to mount FUSE: %w", err)
	}
	m.conn = c

	go m.runFUSE()

	log.Printf("[Volume FUSE] Mounted volume %s at %s", m.volumeID, m.mountPoint)
	return nil
}

// runFUSE starts the FUSE server and blocks until it stops
func (m *VolumeFUSEMount) runFUSE() error {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Volume FUSE] PANIC: %v", r)
		}
	}()

	filesys := &webdavFS{
		baseURL:  m.baseURL,
		username: m.username,
		password: m.password,
	}

	err := fs.Serve(m.conn, filesys)
	log.Printf("[Volume FUSE] fs.Serve returned: %v", err)
	return err
}

// Unmount stops the FUSE mount
func (m *VolumeFUSEMount) Unmount() error {
	if m.conn != nil {
		if err := fuse.Unmount(m.mountPoint); err != nil {
			log.Printf("[Volume FUSE] Warning: failed to unmount: %v", err)
		}
		m.conn.Close()
	}
	os.Remove(m.mountPoint)
	log.Printf("[Volume FUSE] Unmounted volume %s from %s", m.volumeID, m.mountPoint)
	return nil
}

// --- WebDAV Client ---

type webdavClient struct {
	baseURL  string
	username string
	password string
}

func (c *webdavClient) do(method, path string, body io.Reader, headers map[string]string) (*http.Response, error) {
	u := c.baseURL + path
	log.Printf("[Volume FUSE] HTTP %s %s", method, u)
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.username, c.password)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

// --- WebDAV XML types ---

type multiStatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string       `xml:"href"`
	PropStat []davPropStat `xml:"propstat"`
}

type davPropStat struct {
	Prop   davProp `xml:"prop"`
	Status string  `xml:"status"`
}

type davProp struct {
	DisplayName    string `xml:"displayname"`
	ContentLength  int64  `xml:"getcontentlength"`
	LastModified   string `xml:"getlastmodified"`
	ResourceType   int    `xml:"resourcetype"`
	ContentType    string `xml:"getcontenttype"`
}

// --- FUSE Filesystem ---

type webdavFS struct {
	baseURL  string
	username string
	password string
}

func (f *webdavFS) Root() (fs.Node, error) {
	return &davDir{
		client: &webdavClient{baseURL: f.baseURL, username: f.username, password: f.password},
		path:   "/",
	}, nil
}

// --- Directory ---

type davDir struct {
	client *webdavClient
	path   string
}

func (d *davDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0755
	a.Size = 4096
	a.Mtime = time.Now()
	return nil
}

func (d *davDir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	childPath := d.path + name

	// Try directory first (with trailing slash)
	resp, err := d.client.do("PROPFIND", childPath+"/", nil, map[string]string{
		"Depth": "0",
	})
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == 207 || resp.StatusCode == 200 {
			return &davDir{client: d.client, path: childPath + "/"}, nil
		}
	}

	// Try as file
	resp2, err := d.client.do("PROPFIND", childPath, nil, map[string]string{
		"Depth": "0",
	})
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()

	if resp2.StatusCode == 404 {
		return nil, fuse.ENOENT
	}

	return &davFile{client: d.client, path: childPath}, nil
}

func (d *davDir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[Volume FUSE] PANIC in ReadDirAll: %v", r)
		}
	}()

	entries, err := d.list()
	if err != nil {
		return nil, err
	}

	var result []fuse.Dirent
	for _, entry := range entries {
		kind := fuse.DT_File
		if entry.IsDir {
			kind = fuse.DT_Dir
		}
		result = append(result, fuse.Dirent{
			Name: entry.Name,
			Type: kind,
		})
	}
	return result, nil
}

func (d *davDir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	childPath := d.path + req.Name
	file := &davFile{
		client: d.client,
		path:   childPath,
	}

	// Create empty file via PUT
	if err := file.writeAt([]byte{}, 0); err != nil {
		return nil, nil, err
	}

	return file, file, nil
}

func (d *davDir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	childPath := d.path + req.Name + "/"
	resp, err := d.client.do("MKCOL", childPath, nil, nil)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()

	if resp.StatusCode != 201 && resp.StatusCode != 405 {
		return nil, fmt.Errorf("MKCOL failed: HTTP %d", resp.StatusCode)
	}

	return &davDir{client: d.client, path: childPath}, nil
}

func (d *davDir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	childPath := d.path + req.Name
	resp, err := d.client.do("DELETE", childPath, nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != 204 && resp.StatusCode != 404 {
		return fmt.Errorf("DELETE failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (d *davDir) list() ([]davEntry, error) {
	log.Printf("[Volume FUSE] PROPFIND %s Depth:1", d.client.baseURL+d.path)
	resp, err := d.client.do("PROPFIND", d.path, nil, map[string]string{
		"Depth": "1",
	})
	if err != nil {
		log.Printf("[Volume FUSE] PROPFIND error: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 207 && resp.StatusCode != 200 {
		log.Printf("[Volume FUSE] PROPFIND failed: HTTP %d", resp.StatusCode)
		return nil, fmt.Errorf("PROPFIND failed: HTTP %d", resp.StatusCode)
	}

	var ms multiStatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("failed to parse PROPFIND response: %w", err)
	}

	// Skip the first entry (the directory itself)
	var entries []davEntry
	for i, r := range ms.Responses {
		if i == 0 {
			continue // skip self
		}
		name := pathBase(r.Href)
		if name == "" || name == "." {
			continue
		}
		isDir := false
		if len(r.PropStat) > 0 {
			isDir = r.PropStat[0].Prop.ResourceType == 1
		}
		entries = append(entries, davEntry{Name: name, IsDir: isDir})
	}
	return entries, nil
}

type davEntry struct {
	Name  string
	IsDir bool
}

func pathBase(href string) string {
	href = strings.TrimSuffix(href, "/")
	parts := strings.Split(href, "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// --- File ---

type davFile struct {
	client *webdavClient
	path   string
}

func (f *davFile) Attr(ctx context.Context, a *fuse.Attr) error {
	resp, err := f.client.do("PROPFIND", f.path, nil, map[string]string{
		"Depth": "0",
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 207 && resp.StatusCode != 200 {
		a.Mode = 0644
		a.Size = 0
		return nil
	}

	var ms multiStatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return err
	}

	if len(ms.Responses) > 0 && len(ms.Responses[0].PropStat) > 0 {
		prop := ms.Responses[0].PropStat[0].Prop
		a.Size = uint64(prop.ContentLength)
		a.Mode = 0644
		if t, err := time.Parse(time.RFC1123, prop.LastModified); err == nil {
			a.Mtime = t
		}
	}

	return nil
}

func (f *davFile) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	end := req.Offset + int64(req.Size)
	rangeHeader := fmt.Sprintf("bytes=%d-%d", req.Offset, end-1)

	httpResp, err := f.client.do("GET", f.path, nil, map[string]string{
		"Range": rangeHeader,
	})
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != 200 && httpResp.StatusCode != 206 {
		return fmt.Errorf("GET failed: HTTP %d", httpResp.StatusCode)
	}

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return err
	}

	resp.Data = data
	return nil
}

func (f *davFile) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	// For WebDAV, we need to PUT the full content
	// Read existing content first if offset > 0
	var existing []byte
	if req.Offset > 0 {
		httpResp, err := f.client.do("GET", f.path, nil, nil)
		if err == nil {
			existing, _ = io.ReadAll(httpResp.Body)
			httpResp.Body.Close()
		}
	}

	// Build full content
	full := make([]byte, req.Offset+int64(len(req.Data)))
	copy(full, existing)
	copy(full[req.Offset:], req.Data)

	return f.writeAt(full, 0)
}

func (f *davFile) writeAt(data []byte, offset int64) error {
	resp, err := f.client.do("PUT", f.path, strings.NewReader(string(data)), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != 201 && resp.StatusCode != 204 && resp.StatusCode != 200 {
		return fmt.Errorf("PUT failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (f *davFile) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	return nil
}

func (f *davFile) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	return nil
}

// --- Active Mounts ---

var (
	activeMounts   = make(map[string]*VolumeFUSEMount)
	activeMountsMu sync.Mutex
)

func MountVolume(volumeID, mountPoint, baseURL, username, password string) error {
	activeMountsMu.Lock()
	if _, exists := activeMounts[volumeID]; exists {
		activeMountsMu.Unlock()
		return fmt.Errorf("volume %s is already mounted", volumeID)
	}
	activeMountsMu.Unlock()

	mount := NewVolumeFUSEMount(volumeID, mountPoint, baseURL, username, password)
	if err := mount.Mount(); err != nil {
		return err
	}

	activeMountsMu.Lock()
	activeMounts[volumeID] = mount
	activeMountsMu.Unlock()

	return nil
}

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
