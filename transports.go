package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Payout transports.
//
// A credited balance has to leave the pool somehow, and Grin gives no fire-and-forget send.
// grin-wallet resolves a Slatepack address to an onion address and pushes the slate to a
// LISTENING receiver wallet over Tor; if that fails it emits a Slatepack blob for the
// recipient to round-trip by hand. There is no send-to-address. So a pool has exactly two
// ways to pay, and miners choose which they can accept.
//
// This drives them through the grin-wallet CLI rather than its owner API. That is a
// deliberate choice: only /v3/owner still exists in 5.5.0, v1 and v2 both 404, and v3 is an
// ECDH handshake with AES-GCM on every call. Hand-rolling that in Go, in the one code path
// that moves money, to reach a workflow the CLI already implements and that we read in
// grin-wallet's own source, is a bad trade. The CLI is the same code with a tested front
// door.
//
// The passphrase goes in on stdin, never as an argument, for the same reason the systemd
// units do it that way.

// Transport is how a miner's payout leaves the pool.
type Transport string

const (
	// TransportTor pushes to the miner's listening wallet. The default, and the only one
	// that needs nothing from the miner at payout time beyond being online.
	TransportTor Transport = "tor"
	// TransportSlatepack writes a blob for the miner to claim and return. Works for a miner
	// who will not run a listener, at the cost of a manual round trip per payout.
	TransportSlatepack Transport = "slatepack"
)

// nanogrin per GRIN. The CLI takes decimal GRIN; balances are integers of this unit.
const nanogrinPerGrin uint64 = 1_000_000_000

var (
	ErrNoDestination = errors.New("payout has no destination address")
	ErrNoTxID        = errors.New("payout has no wallet transaction to cancel")
	ErrZeroAmount    = errors.New("payout amount is zero")
	ErrSendFailed    = errors.New("wallet refused the send")
)

// Sender runs payouts through a grin-wallet binary against one wallet directory.
type Sender struct {
	Bin       string        // grin-wallet
	WalletDir string        // -t, the wallet's top level dir
	PassFile  string        // read and piped to stdin; never an argument
	MinConf   int           // -c, confirmations an output needs to be spendable
	Timeout   time.Duration // a Tor round trip can be slow; it must not be unbounded
}

// FormatGrin renders a nanogrin amount as the decimal GRIN string the CLI expects.
//
// Integer arithmetic throughout. A float64 holds a 60 GRIN reward exactly, but it does not
// hold every nanogrin split of one, and a payout path is the wrong place to find out which
// values round. Trailing zeros are trimmed because the CLI takes "1.5" and "1.500000000"
// alike, and the shorter form is what a human checking a log expects to see.
func FormatGrin(nano uint64) string {
	whole := nano / nanogrinPerGrin
	frac := nano % nanogrinPerGrin
	if frac == 0 {
		return strconv.FormatUint(whole, 10)
	}
	s := fmt.Sprintf("%d.%09d", whole, frac)
	return strings.TrimRight(s, "0")
}

// args builds the exact command line for a payout. Separated from running it so the shape
// can be asserted in a test without a wallet, funds, or a network.
func (s *Sender) args(t Transport, dest string, nano uint64, outfile string) []string {
	a := []string{"-t", s.WalletDir, "send", "-d", dest}
	if s.MinConf > 0 {
		a = append(a, "-c", strconv.Itoa(s.MinConf))
	}
	if t == TransportSlatepack {
		// -m stops it attempting Tor, so the blob is produced rather than a send tried and
		// failed; -u puts the blob where the claim page can find it.
		a = append(a, "-m", "-u", outfile)
	}
	return append(a, FormatGrin(nano))
}

// Send pays one miner. For TransportTor the wallet completes the transaction against the
// miner's listener; for TransportSlatepack it writes a blob to outfile and the payout is not
// complete until the miner returns it.
func (s *Sender) Send(ctx context.Context, t Transport, dest string, nano uint64, outfile string) (string, error) {
	if strings.TrimSpace(dest) == "" {
		return "", ErrNoDestination
	}
	if nano == 0 {
		return "", ErrZeroAmount
	}

	pass, err := os.ReadFile(s.PassFile)
	if err != nil {
		return "", fmt.Errorf("read passphrase file %s: %w", s.PassFile, err)
	}

	timeout := s.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.Bin, s.args(t, dest, nano, outfile)...)
	cmd.Stdin = bytes.NewReader(pass)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	runErr := cmd.Run()
	text := out.String()

	// grin-wallet exits 0 having only emitted a Slatepack when a Tor send fails, so the exit
	// code alone does not tell us whether the miner was paid. The log line does.
	if t == TransportTor && strings.Contains(text, "Unable to send transaction via TOR") {
		return text, fmt.Errorf("%w: tor push failed, the miner's listener did not answer", ErrSendFailed)
	}
	if runErr != nil {
		return text, fmt.Errorf("%w: %v", ErrSendFailed, runErr)
	}
	return text, nil
}


// slateIDPattern finds the wallet's transaction UUID in send output. Cancelling needs it,
// and a slatepack payout that cannot be cancelled is one the pool can never reclaim.
var slateIDPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// SlateIDFrom pulls the transaction UUID out of grin-wallet's send output, or "" if the
// output does not carry one.
func SlateIDFrom(out string) string {
	return slateIDPattern.FindString(out)
}

// Cancel unlocks the outputs behind an unconfirmed transaction, so the amount can be
// credited back and spent again.
//
// This is what makes a slatepack payout reclaimable. Without it an unclaimed blob commits
// the pool's funds forever: the outputs stay locked, the miner never finalises, and nobody
// can spend them.
//
// Order matters at the call site. Cancel first, then credit the miner back. Cancelling
// invalidates the slate the miner holds, so after it succeeds they cannot complete it; the
// reverse order would leave a window where a miner could both claim the slatepack and hold
// the returned balance.
func (s *Sender) Cancel(ctx context.Context, txID string) (string, error) {
	if strings.TrimSpace(txID) == "" {
		return "", ErrNoTxID
	}
	pass, err := os.ReadFile(s.PassFile)
	if err != nil {
		return "", fmt.Errorf("read passphrase file %s: %w", s.PassFile, err)
	}

	timeout := s.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.Bin, "-t", s.WalletDir, "cancel", "-t", txID)
	cmd.Stdin = bytes.NewReader(pass)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("cancel %s: %w", txID, err)
	}
	return out.String(), nil
}
