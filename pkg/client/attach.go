package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/containers/conmon-rs/internal/proto"
	"github.com/containers/podman/v4/libpod/define"
	"github.com/containers/podman/v4/pkg/kubeutils"
	"github.com/containers/podman/v4/utils"
)

const (
	attachPacketBufSize = 8192
	attachPipeStdin     = 1 // nolint:deadcode,varcheck // Not used right now
	attachPipeStdout    = 2
	attachPipeStderr    = 3
)

var (
	errOutputDestNil   = errors.New("output destination cannot be nil")
	errTerminalSizeNil = errors.New("terminal size cannot be nil")
)

// AttachStreams are the stdio streams for the AttachConfig.
type AttachStreams struct {
	// Standard input stream, can be nil.
	Stdin *In

	// Standard output stream, can be nil.
	Stdout *Out

	// Standard error stream, can be nil.
	Stderr *Out
}

// In defines an input stream.
type In struct {
	// Wraps an io.Reader
	io.Reader
}

// Out defines an output stream.
type Out struct {
	// Wraps an io.WriteCloser
	io.WriteCloser
}

// AttachConfig is the configuration for running the Attach method.
type AttachConfig struct {
	// ID of the container.
	ID string

	// Path of the attach socket.
	SocketPath string

	// ExecSession ID, if this is an attach for an Exec.
	ExecSession string

	// Whether a terminal was setup for the command this is attaching to.
	Tty bool

	// Whether stdout/stderr should continue to be processed after stdin is closed.
	StopAfterStdinEOF bool

	// Whether the output is passed through the caller's std streams, rather than
	// ones created for the attach session.
	Passthrough bool

	// Channel of resize events.
	Resize chan define.TerminalSize

	// The standard streams for this attach session.
	Streams AttachStreams

	// A closure to be run before the streams are attached.
	// This could be used to start a container.
	PreAttachFunc func() error

	// A closure to be run after the streams are attached.
	// This could be used to notify callers the streams have been attached.
	PostAttachFunc func() error

	// The keys that indicate the attach session should be detached.
	DetachKeys []byte
}

// AttachContainer can be used to attach to a running container.
func (c *ConmonClient) AttachContainer(ctx context.Context, cfg *AttachConfig) error {
	conn, err := c.newRPCConn()
	if err != nil {
		return fmt.Errorf("create RPC connection: %w", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			c.logger.Errorf("Unable to close connection: %v", err)
		}
	}()

	client := proto.Conmon{Client: conn.Bootstrap(ctx)}
	future, free := client.AttachContainer(ctx, func(p proto.Conmon_attachContainer_Params) error {
		req, err := p.NewRequest()
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}

		if err := req.SetId(cfg.ID); err != nil {
			return fmt.Errorf("set ID: %w", err)
		}

		if err := req.SetSocketPath(cfg.SocketPath); err != nil {
			return fmt.Errorf("set socket path: %w", err)
		}

		// TODO: add exec session
		return nil
	})
	defer free()

	result, err := future.Struct()
	if err != nil {
		return fmt.Errorf("create result: %w", err)
	}

	if _, err := result.Response(); err != nil {
		return fmt.Errorf("set response: %w", err)
	}

	if err := c.attach(ctx, cfg); err != nil {
		return fmt.Errorf("run attach: %w", err)
	}

	return nil
}

func (c *ConmonClient) attach(ctx context.Context, cfg *AttachConfig) (err error) {
	var conn *net.UnixConn
	if !cfg.Passthrough {
		c.logger.Debugf("Attaching to container %s", cfg.ID)

		kubeutils.HandleResizing(cfg.Resize, func(size define.TerminalSize) {
			c.logger.Debugf("Got a resize event: %+v", size)
			if err := c.SetWindowSizeContainer(ctx, &SetWindowSizeContainerConfig{
				ID:   cfg.ID,
				Size: &size,
			}); err != nil {
				c.logger.Debugf("Failed to write to control file to resize terminal: %v", err)
			}
		})

		conn, err = DialLongSocket("unixpacket", cfg.SocketPath)
		if err != nil {
			return fmt.Errorf("failed to connect to container's attach socket: %v: %w", cfg.SocketPath, err)
		}
		defer func() {
			if err := conn.Close(); err != nil {
				c.logger.Errorf("unable to close socket: %q", err)
			}
		}()
	}

	if cfg.PreAttachFunc != nil {
		if err := cfg.PreAttachFunc(); err != nil {
			return fmt.Errorf("run pre attach func: %w", err)
		}
	}

	if cfg.Passthrough {
		return nil
	}

	receiveStdoutError, stdinDone := c.setupStdioChannels(cfg, conn)
	if cfg.PostAttachFunc != nil {
		if err := cfg.PostAttachFunc(); err != nil {
			return fmt.Errorf("run post attach func: %w", err)
		}
	}

	if err := c.readStdio(cfg, conn, receiveStdoutError, stdinDone); err != nil {
		return fmt.Errorf("read stdio: %w", err)
	}

	return nil
}

func (c *ConmonClient) setupStdioChannels(
	cfg *AttachConfig, conn *net.UnixConn,
) (receiveStdoutError, stdinDone chan error) {
	receiveStdoutError = make(chan error)
	go func() {
		receiveStdoutError <- c.redirectResponseToOutputStreams(cfg, conn)
	}()

	stdinDone = make(chan error)
	go func() {
		var err error
		if cfg.Streams.Stdin != nil {
			_, err = utils.CopyDetachable(conn, cfg.Streams.Stdin, cfg.DetachKeys)
		}
		stdinDone <- err
	}()

	return receiveStdoutError, stdinDone
}

func (c *ConmonClient) redirectResponseToOutputStreams(cfg *AttachConfig, conn io.Reader) (err error) {
	buf := make([]byte, attachPacketBufSize+1) /* Sync with conmonrs ATTACH_PACKET_BUF_SIZE */
	for {
		nr, er := conn.Read(buf)
		if nr > 0 {
			var dst io.Writer
			var doWrite bool
			switch buf[0] {
			case attachPipeStdout:
				dst = cfg.Streams.Stdout
				doWrite = cfg.Streams.Stdout != nil
			case attachPipeStderr:
				dst = cfg.Streams.Stderr
				doWrite = cfg.Streams.Stderr != nil
			default:
				c.logger.Infof("Received unexpected attach type %+d", buf[0])
			}
			if dst == nil {
				return errOutputDestNil
			}

			if doWrite {
				nw, ew := dst.Write(buf[1:nr])
				if ew != nil {
					err = ew

					break
				}
				if nr != nw+1 {
					err = io.ErrShortWrite

					break
				}
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			err = er

			break
		}
	}

	if err != nil {
		return fmt.Errorf("redirect response to output streams: %w", err)
	}

	return nil
}

func (c *ConmonClient) readStdio(
	cfg *AttachConfig, conn *net.UnixConn, receiveStdoutError, stdinDone chan error,
) error {
	var err error
	select {
	case err = <-receiveStdoutError:
		if closeErr := conn.CloseWrite(); closeErr != nil {
			return fmt.Errorf("%v: %w", closeErr, err)
		}

		if err != nil {
			return fmt.Errorf("got stdout error: %w", err)
		}

		return nil

	case err = <-stdinDone:
		// This particular case is for when we get a non-tty attach
		// with --leave-stdin-open=true. We want to return as soon
		// as we receive EOF from the client. However, we should do
		// this only when stdin is enabled. If there is no stdin
		// enabled then we wait for output as usual.
		if cfg.StopAfterStdinEOF {
			return nil
		}
		if errors.Is(err, define.ErrDetach) {
			if closeErr := conn.CloseWrite(); closeErr != nil {
				return fmt.Errorf("%v: %w", closeErr, err)
			}

			return err
		}
		if err == nil {
			// copy stdin is done, close it
			if connErr := conn.CloseWrite(); connErr != nil {
				c.logger.Errorf("Unable to close conn: %v", connErr)
			}
		}
		if cfg.Streams.Stdout != nil || cfg.Streams.Stderr != nil {
			return <-receiveStdoutError
		}
	}

	return nil
}

// SetWindowSizeContainerConfig is the configuration for calling the SetWindowSizeContainer method.
type SetWindowSizeContainerConfig struct {
	// ID specifies the container ID.
	ID string

	// Size is the new terminal size.
	Size *define.TerminalSize
}

// SetWindowSizeContainer can be used to change the window size of a running container.
func (c *ConmonClient) SetWindowSizeContainer(ctx context.Context, cfg *SetWindowSizeContainerConfig) error {
	if cfg.Size == nil {
		return errTerminalSizeNil
	}

	conn, err := c.newRPCConn()
	if err != nil {
		return fmt.Errorf("create RPC connection: %w", err)
	}
	defer conn.Close()
	client := proto.Conmon{Client: conn.Bootstrap(ctx)}

	future, free := client.SetWindowSizeContainer(ctx, func(p proto.Conmon_setWindowSizeContainer_Params) error {
		req, err := p.NewRequest()
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}

		if err := req.SetId(cfg.ID); err != nil {
			return fmt.Errorf("set ID: %w", err)
		}

		req.SetWidth(cfg.Size.Width)
		req.SetHeight(cfg.Size.Height)

		if err := p.SetRequest(req); err != nil {
			return fmt.Errorf("set request: %w", err)
		}

		return nil
	})
	defer free()

	result, err := future.Struct()
	if err != nil {
		return fmt.Errorf("create result: %w", err)
	}

	if _, err := result.Response(); err != nil {
		return fmt.Errorf("set response: %w", err)
	}

	return nil
}
