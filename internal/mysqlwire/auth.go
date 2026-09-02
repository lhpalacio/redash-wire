package mysqlwire

import (
	"crypto/rand"
	"crypto/subtle"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
)

// credentialAuthHandler supplies the single configured username/password to
// go-mysql's DefaultAuthenticationProvider, which performs the actual
// mysql_native_password challenge-response: it hashes the returned password
// and compares it (in constant time) against the client's scramble.
//
// It never reports a username as unknown. go-mysql answers found=false with
// ER_NO_SUCH_USER (1449), which is both the wrong error (real MySQL says 1045
// Access denied for a bad user and a bad password alike) and a username
// oracle. An unknown user is instead handed a decoy password no client can
// know, so its scramble check fails exactly the way a wrong password does.
type credentialAuthHandler struct {
	username string
	password string
	// decoy is the password verified for any username but the configured one.
	// It is random per connection, so it can never equal what a client typed.
	decoy string
	// admit runs once the password has been verified, before the OK packet; a
	// non-nil error is sent to the client instead of the OK.
	admit func(conn *server.Conn) error
}

func newCredentialAuthHandler(username, password string, admit func(conn *server.Conn) error) *credentialAuthHandler {
	return &credentialAuthHandler{
		username: username,
		password: password,
		decoy:    rand.Text(),
		admit:    admit,
	}
}

func (a *credentialAuthHandler) GetCredential(username string) (server.Credential, bool, error) {
	password := a.decoy
	if subtle.ConstantTimeCompare([]byte(username), []byte(a.username)) == 1 {
		password = a.password
	}
	return server.Credential{
		Passwords:      []string{password},
		AuthPluginName: mysql.AUTH_NATIVE_PASSWORD,
	}, true, nil
}

func (a *credentialAuthHandler) OnAuthSuccess(conn *server.Conn) error {
	if a.admit == nil {
		return nil
	}
	return a.admit(conn)
}

func (a *credentialAuthHandler) OnAuthFailure(_ *server.Conn, _ error) {}
