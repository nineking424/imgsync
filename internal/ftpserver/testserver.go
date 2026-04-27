// Package ftpserver runs an in-process FTP server for tests. Not for prod use.
package ftpserver

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"path"
	"testing"

	ftplib "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
)

// Server holds the address and credentials of the running test FTP server.
type Server struct {
	Addr    string
	User    string
	Pass    string
	RootDir string
}

// Start launches an in-process FTP server and returns its connection details.
// It registers a cleanup that stops the server when the test ends.
func Start(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ftpserver: listen: %v", err)
	}

	d := &driver{
		root:     root,
		user:     "imgsync",
		pass:     "imgsync",
		listener: listener,
	}

	srv := ftplib.NewFtpServer(d)

	// Listen() must be called before Serve() to initialize the internal listener.
	if err = srv.Listen(); err != nil {
		_ = listener.Close()
		t.Fatalf("ftpserver: srv.Listen: %v", err)
	}

	go func() {
		_ = srv.Serve()
	}()

	t.Cleanup(func() { _ = srv.Stop() })

	return &Server{
		Addr:    listener.Addr().String(),
		User:    d.user,
		Pass:    d.pass,
		RootDir: root,
	}
}

// driver implements ftplib.MainDriver for test use.
type driver struct {
	root     string
	user     string
	pass     string
	listener net.Listener
}

func (d *driver) GetSettings() (*ftplib.Settings, error) {
	return &ftplib.Settings{
		Listener:            d.listener,
		DefaultTransferType: ftplib.TransferTypeBinary,
		DisableMLSD:         true,
		DisableMLST:         true,
	}, nil
}

func (d *driver) ClientConnected(_ ftplib.ClientContext) (string, error) {
	return "imgsync test ftp", nil
}

func (d *driver) ClientDisconnected(_ ftplib.ClientContext) {}

func (d *driver) AuthUser(_ ftplib.ClientContext, user, pass string) (ftplib.ClientDriver, error) {
	if user != d.user || pass != d.pass {
		return nil, fmt.Errorf("auth: bad credentials")
	}
	return &clientDriver{Fs: afero.NewBasePathFs(afero.NewOsFs(), d.root)}, nil
}

// GetTLSConfig returns an error because TLS is not used in tests.
// The interface requires *tls.Config (not any) in ftpserverlib v0.30+.
func (d *driver) GetTLSConfig() (*tls.Config, error) {
	return nil, errors.New("tls not configured")
}

// clientDriver wraps an afero.Fs and implements ftplib.ClientDriver.
type clientDriver struct {
	afero.Fs
}

// AbsPath cleans and returns an absolute path relative to the virtual root.
func (c *clientDriver) AbsPath(p string) string {
	return path.Clean("/" + p)
}
