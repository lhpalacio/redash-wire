package mysqlwire

import (
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
)

// credentialAuthHandler supplies the single configured username/password to
// go-mysql's DefaultAuthenticationProvider, which performs the actual
// mysql_native_password challenge-response: it hashes the returned password
// and compares it against the client's scramble. Returning found=false for an
// unknown username fails the handshake before any password comparison.
type credentialAuthHandler struct {
	username string
	password string
}

func (a *credentialAuthHandler) GetCredential(username string) (server.Credential, bool, error) {
	if username != a.username {
		return server.Credential{}, false, nil
	}
	return server.Credential{
		Passwords:      []string{a.password},
		AuthPluginName: mysql.AUTH_NATIVE_PASSWORD,
	}, true, nil
}

func (a *credentialAuthHandler) OnAuthSuccess(_ *server.Conn) error    { return nil }
func (a *credentialAuthHandler) OnAuthFailure(_ *server.Conn, _ error) {}
