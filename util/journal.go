package util

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
)

var journalSocket = &net.UnixAddr{Name: "/run/systemd/journal/socket", Net: "unixgram"}
var journalConnection *net.UnixConn

// https://systemd.io/JOURNAL_NATIVE_PROTOCOL/
// http://www.freedesktop.org/software/systemd/man/systemd.journal-fields.html
func JournalLog(msg, priority string, kvs map[string]string) error {
	if journalConnection == nil {
		c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Net: "unixgram"})
		if err != nil {
			return err
		}
		journalConnection = c
	}
	w := &bytes.Buffer{}
	writeJournalKV(w, "MESSAGE", msg)
	writeJournalKV(w, "PRIORITY", priority)
	for k, v := range kvs {
		writeJournalKV(w, k, v)
	}
	_, _, err := journalConnection.WriteMsgUnix(w.Bytes(), nil, journalSocket)
	return err
}

func writeJournalKV(w io.Writer, k, v string) {
	if !strings.ContainsRune(v, '\n') {
		fmt.Fprintf(w, "%s=%s\n", k, v)
	} else {
		fmt.Fprint(w, k)
		binary.Write(w, binary.LittleEndian, uint64(len(v)))
		fmt.Fprint(w, v)
	}
}
