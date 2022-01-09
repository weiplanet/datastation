package runner

import (
	"bufio"
	"compress/gzip"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ADAPTED: https://gist.github.com/stefanprodan/2d20d0c6fdab6f14ce8219464e8b4b9a
func getSSHPrivateKeySigner(privateKeyFile, passphrase string) (ssh.AuthMethod, error) {
	pemBytes, err := ioutil.ReadFile(resolvePath(privateKeyFile))
	if err != nil {
		return nil, edsef("Unable to read private key: %s", err)
	}

	pemBlock, _ := pem.Decode(pemBytes)
	if pemBlock == nil {
		return nil, edsef("Private key decode failed")
	}

	// Handle encrypted private keys
	if x509.IsEncryptedPEMBlock(pemBlock) {
		pemBlock.Bytes, err = x509.DecryptPEMBlock(pemBlock, []byte(passphrase))
		if err != nil {
			return nil, edsef("Decrypting private key failed: %s", err)
		}

		key, err := parsePemBlock(pemBlock)
		if err != nil {
			return nil, err
		}

		signer, err := ssh.NewSignerFromKey(key)
		if err != nil {
			return nil, edsef("Creating signer from encrypted key failed: %s", err)
		}

		return ssh.PublicKeys(signer), nil
	}

	// Not encrypted
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, edsef("Parsing plain private key failed: %s", err)
	}

	return ssh.PublicKeys(signer), nil
}

// ssh.NewSignerFromKey actually takes an interface{}... so great.
func parsePemBlock(block *pem.Block) (interface{}, error) {
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, edsef("Parsing PKCS private key failed: %s", err)
		}

		return key, nil
	case "EC PRIVATE KEY":
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, edsef("Parsing EC private key failed: %s", err)
		}

		return key, nil
	case "DSA PRIVATE KEY":
		key, err := ssh.ParseDSAPrivateKey(block.Bytes)
		if err != nil {
			return nil, edsef("Parsing DSA private key failed: %s", err)
		}

		return key, nil
	}

	return nil, edsef("Unsupported private key type: %s", block.Type)
}

var defaultKeyFiles = []string{
	"~/.ssh/id_rsa",
	"~/.ssh/id_dsa",
	"~/.ssh/id_ed25519",
}

func getSSHClient(si ServerInfo) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User: si.Username,
		// TODO: figure out if we want to validate host keys
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	switch si.Type {
	case SSHPassword:
		password, err := si.Password.decrypt()
		if err != nil {
			return nil, edsef("Could not decrypt server SSH password: " + err.Error())
		}
		config.Auth = []ssh.AuthMethod{ssh.Password(password)}
	case SSHPrivateKey:
		if si.PrivateKeyFile != "" {
			passphrase, err := si.Passphrase.decrypt()
			if err != nil {
				return nil, edsef("Could not decrypt server SSH passphrase: " + err.Error())
			}
			authmethod, err := getSSHPrivateKeySigner(si.PrivateKeyFile, passphrase)
			if err != nil {
				return nil, err
			}
			config.Auth = []ssh.AuthMethod{authmethod}
		} else {
			// Try all default path ssh files
			for _, f := range defaultKeyFiles {
				resolved := resolvePath(f)
				if _, err := os.Stat(resolved); err == nil {
					authmethod, err := getSSHPrivateKeySigner(resolved, "")
					if err != nil {
						return nil, err
					}
					config.Auth = []ssh.AuthMethod{authmethod}
				}
			}
		}
	default:
		return nil, edsef("SSH Agent authentication is not supported yet.")
	}

	if !strings.Contains(si.Address, ":") {
		si.Address = si.Address + ":22"
	}

	conn, err := ssh.Dial("tcp", si.Address, config)
	if err != nil {
		return nil, edsef("Could not connect to remote server: %s", err)
	}

	return conn, nil
}

func remoteFileReader(si ServerInfo, remoteFileName string, callback func(r io.Reader) error) error {
	client, err := getSSHClient(si)
	if err != nil {
		return err
	}

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	r, err := session.StdoutPipe()
	if err != nil {
		return edsef("Could not create stdout pipe: %s", err)
	}

	cmd := fmt.Sprintf(`if command -v gzip > /dev/null 2>&1; then
  cat %s | gzip
else
  cat %s
fi`, remoteFileName, remoteFileName)
	if err := session.Start(cmd); err != nil {
		return edsef("Could not start session command: %s", err)
	}

	// Clone the stream to peak if it is gzipped
	buffer := bufio.NewReader(r)
	magicBytes, err := buffer.Peek(2)
	if err != nil {
		return edsef("Could not read magic number from stream: %s", err)
	}

	// Set default reader to be plain text
	var reader io.Reader = buffer

	// If it is gzipped, set the reader to be a gzip reader
	if magicBytes[0] == 0x1F && magicBytes[1] == 0x8B {
		fz, err := gzip.NewReader(buffer)
		if err != nil {
			return edsef("Could not create gzip reader: %s", err)
		}

		reader = fz
		defer fz.Close()
	}

	err = callback(reader)
	if err != nil {
		return err
	}

	if err := session.Wait(); err != nil {
		return edsef("Could not complete session: %s", err)
	}

	return nil
}

func withRemoteConnection(si *ServerInfo, host, port string, cb func(host, port string) error) error {
	if si == nil {
		return cb(host, port)
	}

	// Pick any open port
	localConn, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return err
	}
	defer localConn.Close()

	client, err := getSSHClient(*si)
	if err != nil {
		return err
	}

	remoteConn, err := client.Dial("tcp", host+":"+port)
	if err != nil {
		return err
	}
	defer remoteConn.Close()

	errC := make(chan error)

	// Local server
	go func() {
		localConn, err := localConn.Accept()
		if err != nil {
			errC <- err
			return
		}

		go func() {
			_, err = io.Copy(remoteConn, localConn)
			if errC != nil {
				errC <- err
			}

		}()

		// Remote server
		go func() {
			_, err = io.Copy(localConn, remoteConn)
			errC <- err
		}()
	}()

	localPort := localConn.Addr().(*net.TCPAddr).Port
	cbErr := cb("localhost", fmt.Sprintf("%d", localPort))
	if cbErr != nil {
		return cbErr
	}

	select {
	case err = <-errC:
		if err == io.EOF {
			return nil
		}

		return err
	default:
		return nil
	}
}