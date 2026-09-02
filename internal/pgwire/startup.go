package pgwire

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net"

	"github.com/jackc/pgx/v5/pgproto3"
)

var ErrAuthFailed = fmt.Errorf("authentication failed")

// ErrRefused reports that a valid login was turned away by the admission check.
// The client has already been told why, over the wire.
var ErrRefused = errors.New("connection refused")

// ErrCancelRequest reports that the connection carried a PostgreSQL
// CancelRequest rather than a normal startup. It is not a fault: PostgreSQL
// cancellation arrives on a throwaway connection that names the session to cancel
// (by the ProcessID/SecretKey pair from its BackendKeyData) and is then closed.
// The handler has already asked for the cancellation, so the caller should simply
// close this connection.
var ErrCancelRequest = errors.New("cancel request")

// CancelFunc cancels the in-flight query of the session identified by a
// CancelRequest. The proxy looks the session up by the ProcessID/SecretKey pair
// it handed out in BackendKeyData.
type CancelFunc func(processID uint32, secretKey []byte)

// SQLStateConnectionFailure (08006) says the fault is upstream of the proxy,
// rather than in the client's credentials or its SQL.
const SQLStateConnectionFailure = "08006"

// Admit runs after the client authenticates and before the server declares itself
// ready. Returning an error refuses the connection, and that error's message is
// what the client is shown.
//
// It has to happen inside the startup exchange. Once ReadyForQuery reaches the
// wire, pgx and libpq both consider the connection established: a FATAL sent
// after that point surfaces as a failed *query* on a connection the client
// believes it opened successfully, which is not a refusal at all.
type Admit func() error

// MaxClientMessageBytes caps the declared body length of a single client protocol
// message. pgproto3 checks this before allocating, so a hostile client cannot make
// the proxy allocate gigabytes by declaring a huge message body (e.g. pre-auth).
// 64 MiB is far larger than any legitimate query string yet bounds the blast radius.
const MaxClientMessageBytes = 64 << 20

// HandleStartup runs the PostgreSQL startup phase: it refuses SSL/GSS encryption
// upgrades, authenticates the client against the configured credentials, and sends
// the post-auth parameter bundle through ReadyForQuery. It returns the client's
// startup parameters (user, database, application_name, ...) for the session to use.
func HandleStartup(backend *pgproto3.Backend, conn net.Conn, username, password string, admit Admit, key pgproto3.BackendKeyData, onCancel CancelFunc) (map[string]string, error) {
	for {
		startupMsg, err := backend.ReceiveStartupMessage()
		if err != nil {
			return nil, fmt.Errorf("receiving startup message: %w", err)
		}

		switch msg := startupMsg.(type) {
		case *pgproto3.SSLRequest:
			if _, err := conn.Write([]byte{'N'}); err != nil {
				return nil, fmt.Errorf("denying SSL: %w", err)
			}
			continue

		case *pgproto3.GSSEncRequest:
			if _, err := conn.Write([]byte{'N'}); err != nil {
				return nil, fmt.Errorf("denying GSSENC: %w", err)
			}
			continue

		case *pgproto3.CancelRequest:
			// A throwaway connection asking us to cancel another session's query.
			// Ask the server to do so, then report ErrCancelRequest so the caller
			// closes this connection without treating it as a failed handshake.
			if onCancel != nil {
				onCancel(msg.ProcessID, msg.SecretKey)
			}
			return nil, ErrCancelRequest

		case *pgproto3.StartupMessage:
			params := msg.Parameters

			if err := authenticate(backend, conn, params["user"], username, password); err != nil {
				return nil, err
			}

			// After authentication, so a port scanner cannot read the proxy's
			// health off an unauthenticated connection.
			if admit != nil {
				if err := admit(); err != nil {
					_ = SendFatal(conn, SQLStateConnectionFailure, err.Error())
					return nil, fmt.Errorf("%w: %w", ErrRefused, err)
				}
			}

			buf, err := encode((&pgproto3.AuthenticationOk{}).Encode(nil))
			if err != nil {
				return nil, err
			}

			for _, ps := range parameterStatuses() {
				buf, err = encode(ps.Encode(buf))
				if err != nil {
					return nil, err
				}
			}

			// The ProcessID/SecretKey the server assigned this session. The client
			// echoes them back on a CancelRequest to name the query to cancel.
			buf, err = encode((&pgproto3.BackendKeyData{
				ProcessID: key.ProcessID,
				SecretKey: key.SecretKey,
			}).Encode(buf))
			if err != nil {
				return nil, err
			}

			buf, err = encode((&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(buf))
			if err != nil {
				return nil, err
			}

			if _, err := conn.Write(buf); err != nil {
				return nil, fmt.Errorf("sending startup response: %w", err)
			}

			return params, nil

		default:
			return nil, fmt.Errorf("unexpected startup message: %T", msg)
		}
	}
}

func authenticate(backend *pgproto3.Backend, conn net.Conn, clientUser, expectedUser, expectedPassword string) error {
	// Cleartext (rather than MD5/SCRAM) so the proxy can compare the client's
	// password directly against the configured plaintext value below.
	buf, err := encode((&pgproto3.AuthenticationCleartextPassword{}).Encode(nil))
	if err != nil {
		return err
	}
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("requesting password: %w", err)
	}

	msg, err := backend.Receive()
	if err != nil {
		return fmt.Errorf("receiving password: %w", err)
	}

	pwMsg, ok := msg.(*pgproto3.PasswordMessage)
	if !ok {
		return fmt.Errorf("expected PasswordMessage, got %T", msg)
	}

	// Constant-time comparison so response latency does not leak how many leading
	// bytes of the username/password matched. Both comparisons always run.
	userOK := subtle.ConstantTimeCompare([]byte(clientUser), []byte(expectedUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pwMsg.Password), []byte(expectedPassword)) == 1
	if !userOK || !passOK {
		errBuf, encErr := encode((&pgproto3.ErrorResponse{
			Severity: "FATAL",
			Code:     "28P01",
			Message:  fmt.Sprintf("password authentication failed for user %q", clientUser),
		}).Encode(nil))
		if encErr == nil {
			_, _ = conn.Write(errBuf)
		}
		return ErrAuthFailed
	}

	return nil
}

func parameterStatuses() []pgproto3.ParameterStatus {
	return []pgproto3.ParameterStatus{
		{Name: "server_version", Value: "14.0"},
		{Name: "server_encoding", Value: "UTF8"},
		{Name: "client_encoding", Value: "UTF8"},
		{Name: "DateStyle", Value: "ISO, MDY"},
		{Name: "TimeZone", Value: "UTC"},
		{Name: "standard_conforming_strings", Value: "on"},
		{Name: "integer_datetimes", Value: "on"},
	}
}

func encode(buf []byte, err error) ([]byte, error) {
	if err != nil {
		return nil, fmt.Errorf("encoding message: %w", err)
	}
	return buf, nil
}
