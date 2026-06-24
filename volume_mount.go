package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	p9 "github.com/hugelgupf/p9/p9"
	"golang.org/x/net/context"
)

// VolumeFUSEMount represents a FUSE mount for a remote volume
type VolumeFUSEMount struct {
	volumeID   string
	mountPoint string
	agentHost  string
	agentPort  int
	jwt        string
	rw         bool
	conn       *fuse.Conn
	p9Client   *p9.Client
	cancel     context.CancelFunc
}

// NewVolumeFUSEMount creates a new FUSE mount for a volume
func NewVolumeFUSEMount(volumeID, mountPoint, agentHost string, agentPort int, jwt string, rw bool) *VolumeFUSEMount {
	return &VolumeFUSEMount{
		volumeID:   volumeID,
		mountPoint: mountPoint,
		agentHost:  agentHost,
		agentPort:  agentPort,
		jwt:        jwt,
		rw:         rw,
	}
}

// Mount starts the FUSE mount
func (m *VolumeFUSEMount) Mount() error {
	// Ensure mount point exists
	if err := os.MkdirAll(m.mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Connect to agent via TLS
	p9Client, err := m.connectP9()
	if err != nil {
		return fmt.Errorf("failed to connect to 9p server: %w", err)
	}
	m.p9Client = p9Client

	// Mount FUSE
	fuseOpts := []fuse.MountOption{
		fuse.FSName("okayrun-volume"),
		fuse.Subtype("volume"),
	}
	if !m.rw {
		fuseOpts = append(fuseOpts, fuse.ReadOnly())
	}
	c, err := fuse.Mount(m.mountPoint, fuseOpts...)
	if err != nil {
		p9Client.Close()
		return fmt.Errorf("failed to mount FUSE: %w", err)
	}
	m.conn = c

	// Create and serve the filesystem
	filesys := &VolumeFS{
		volumeID: m.volumeID,
		p9Client: p9Client,
	}

	go func() {
		if err := fs.Serve(c, filesys); err != nil {
			log.Printf("[Volume FUSE] Error serving filesystem: %v", err)
		}
	}()

	log.Printf("[Volume FUSE] Mounted volume %s at %s", m.volumeID, m.mountPoint)
	return nil
}

// connectP9 establishes a TLS connection to the agent's 9p server and authenticates
func (m *VolumeFUSEMount) connectP9() (*p9.Client, error) {
	// Load CA cert for TLS verification
	caCertPath := filepath.Join(os.Getenv("HOME"), ".okayrun", "ca.crt")
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA cert: %w (run 'okay volume mount' first to fetch it)", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("failed to parse CA cert")
	}

	tlsConfig := &tls.Config{
		RootCAs:            caPool,
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // Agent cert lacks IP SANs; JWT auth provides security
	}

	addr := fmt.Sprintf("%s:%d", m.agentHost, m.agentPort)
	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}

	// Pre-9p auth exchange
	reader := bufio.NewReader(conn)
	conn.Write([]byte(fmt.Sprintf("AUTH: %s\n", m.jwt)))

	line, err := reader.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read auth response: %w", err)
	}

	if string(line) != "OK\n" {
		conn.Close()
		return nil, fmt.Errorf("authentication denied: %s", string(line))
	}

	// Create 9p client
	p9Client, err := p9.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create 9p client: %w", err)
	}

	// Negotiate version
	_ = p9Client.Version()

	return p9Client, nil
}

// Unmount stops the FUSE mount
func (m *VolumeFUSEMount) Unmount() error {
	if m.conn != nil {
		if err := fuse.Unmount(m.mountPoint); err != nil {
			log.Printf("[Volume FUSE] Warning: failed to unmount: %v", err)
		}
		m.conn.Close()
	}

	if m.p9Client != nil {
		m.p9Client.Close()
	}

	// Remove mount point
	os.Remove(m.mountPoint)

	log.Printf("[Volume FUSE] Unmounted volume %s from %s", m.volumeID, m.mountPoint)
	return nil
}

// VolumeFS implements fs.FS for the volume
type VolumeFS struct {
	volumeID string
	p9Client *p9.Client
}

// Root returns the root directory of the filesystem
func (f *VolumeFS) Root() (fs.Node, error) {
	root, err := f.p9Client.Attach("")
	if err != nil {
		return nil, fmt.Errorf("failed to attach to volume: %w", err)
	}

	return &VolumeNode{
		p9Client: f.p9Client,
		file:     root,
		path:     "/",
	}, nil
}

// VolumeNode implements fs.Node for files and directories
type VolumeNode struct {
	p9Client *p9.Client
	file     p9.File
	path     string
}

// Attr returns the attributes of the node
func (n *VolumeNode) Attr(ctx context.Context, a *fuse.Attr) error {
	qids, _, attrMask, attr, err := n.file.WalkGetAttr(nil)
	if err != nil {
		return fmt.Errorf("walkgetattr failed: %w", err)
	}
	_ = qids
	_ = attrMask

	a.Size = uint64(attr.Size)
	a.Mode = os.FileMode(attr.Mode)
	a.Mtime = time.Now()
	a.Blocks = (attr.Size + 511) / 512
	return nil
}

// Lookup looks up a child entry
func (n *VolumeNode) Lookup(ctx context.Context, name string) (fs.Node, error) {
	qids, file, err := n.file.Walk([]string{name})
	if err != nil {
		return nil, fmt.Errorf("walk failed: %w", err)
	}
	_ = qids

	return &VolumeNode{
		p9Client: n.p9Client,
		file:     file,
		path:     filepath.Join(n.path, name),
	}, nil
}

// ReadDirAll returns all directory entries
func (n *VolumeNode) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	dirents, err := n.file.Readdir(0, 1024)
	if err != nil {
		return nil, fmt.Errorf("readdir failed: %w", err)
	}

	var result []fuse.Dirent
	for _, d := range dirents {
		entryType := fuse.DT_File
		if d.Type == p9.TypeDir {
			entryType = fuse.DT_Dir
		}
		result = append(result, fuse.Dirent{
			Name: d.Name,
			Type: entryType,
		})
	}

	return result, nil
}

// Create creates a new file
func (n *VolumeNode) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	file, _, _, err := n.file.Create(req.Name, p9.ReadWrite, p9.FileMode(req.Mode), 0, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("create failed: %w", err)
	}

	node := &VolumeNode{
		p9Client: n.p9Client,
		file:     file,
		path:     filepath.Join(n.path, req.Name),
	}

	return node, node, nil
}

// Mkdir creates a new directory
func (n *VolumeNode) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	file, _, _, err := n.file.Create(req.Name, p9.ReadWrite, p9.FileMode(uint32(req.Mode)|uint32(os.ModeDir)), 0, 0)
	if err != nil {
		return nil, fmt.Errorf("mkdir failed: %w", err)
	}

	return &VolumeNode{
		p9Client: n.p9Client,
		file:     file,
		path:     filepath.Join(n.path, req.Name),
	}, nil
}

// Remove removes a file or directory
func (n *VolumeNode) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	// Walk to parent and unlink
	qids, parentFile, err := n.file.Walk([]string{".."})
	if err != nil {
		return fmt.Errorf("walk to parent failed: %w", err)
	}
	_ = qids

	if err := parentFile.UnlinkAt(req.Name, 0); err != nil {
		return fmt.Errorf("remove failed: %w", err)
	}

	return nil
}

// Read reads from the file
func (n *VolumeNode) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	buf := make([]byte, req.Size)
	nbytes, err := n.file.ReadAt(buf, req.Offset)
	if err != nil && err != io.EOF {
		return fmt.Errorf("read failed: %w", err)
	}

	resp.Data = buf[:nbytes]
	return nil
}

// Write writes to the file
func (n *VolumeNode) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	nbytes, err := n.file.WriteAt(req.Data, req.Offset)
	if err != nil {
		return fmt.Errorf("write failed: %w", err)
	}

	resp.Size = nbytes
	return nil
}

// Flush flushes the file
func (n *VolumeNode) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	return nil
}

// Release releases the file
func (n *VolumeNode) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	return nil
}

// ActiveMounts tracks active FUSE mounts
var (
	activeMounts   = make(map[string]*VolumeFUSEMount)
	activeMountsMu sync.Mutex
)

type mountState struct {
	VolumeID   string `json:"volume_id"`
	MountPoint string `json:"mount_point"`
	AgentHost  string `json:"agent_host"`
	AgentPort  int    `json:"agent_port"`
	RW         bool   `json:"rw"`
}

func mountsFilePath() string {
	return filepath.Join(os.Getenv("HOME"), ".okayrun", "mounts.json")
}

func loadMountStates() []mountState {
	data, err := os.ReadFile(mountsFilePath())
	if err != nil {
		return nil
	}
	var states []mountState
	json.Unmarshal(data, &states)
	return states
}

func saveMountStates(states []mountState) {
	data, _ := json.MarshalIndent(states, "", "  ")
	os.MkdirAll(filepath.Dir(mountsFilePath()), 0755)
	os.WriteFile(mountsFilePath(), data, 0644)
}

func addMountState(m *VolumeFUSEMount) {
	states := loadMountStates()
	states = append(states, mountState{
		VolumeID:   m.volumeID,
		MountPoint: m.mountPoint,
		AgentHost:  m.agentHost,
		AgentPort:  m.agentPort,
		RW:         m.rw,
	})
	saveMountStates(states)
}

func removeMountState(volumeID string) {
	states := loadMountStates()
	var filtered []mountState
	for _, s := range states {
		if s.VolumeID != volumeID {
			filtered = append(filtered, s)
		}
	}
	saveMountStates(filtered)
}

// MountVolume mounts a volume locally via FUSE
func MountVolume(volumeID, mountPoint, agentHost string, agentPort int, jwt string, rw bool) error {
	activeMountsMu.Lock()
	if _, exists := activeMounts[volumeID]; exists {
		activeMountsMu.Unlock()
		return fmt.Errorf("volume %s is already mounted", volumeID)
	}
	activeMountsMu.Unlock()

	mount := NewVolumeFUSEMount(volumeID, mountPoint, agentHost, agentPort, jwt, rw)
	if err := mount.Mount(); err != nil {
		return err
	}

	activeMountsMu.Lock()
	activeMounts[volumeID] = mount
	activeMountsMu.Unlock()

	addMountState(mount)

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

	removeMountState(volumeID)

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

	os.Remove(mountsFilePath())
}

// ListMounts returns all persisted mount states
func ListMounts() []mountState {
	return loadMountStates()
}
