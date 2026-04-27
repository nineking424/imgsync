package ftpserver_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/jlaffaye/ftp"
	"github.com/nineking424/imgsync/internal/ftpserver"
	"github.com/stretchr/testify/require"
)

func TestStartTestServer_StoreThenRetrieve(t *testing.T) {
	srv := ftpserver.Start(t)

	c, err := ftp.Dial(srv.Addr)
	require.NoError(t, err)
	defer func() { _ = c.Quit() }()
	require.NoError(t, c.Login(srv.User, srv.Pass))

	require.NoError(t, c.Stor("hello.txt", strings.NewReader("hello world")))

	r, err := c.Retr("hello.txt")
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.True(t, bytes.Equal(got, []byte("hello world")), "got=%q", got)
}
