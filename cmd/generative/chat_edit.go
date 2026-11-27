package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/starfall-org/generative-server/internal/chatstore"
	"github.com/starfall-org/generative-server/internal/node"
)

// editSyncedChat opens an existing opt-in JSON chat in a private temporary
// directory. The active server is not updated directly: every edit goes
// through the same validated, owner-scoped revision CAS as session.save.
func editSyncedChat(store *chatstore.Store, owner, id string, expected uint64, editor string, dryRun, approved bool) error {
	snap, err := store.Get(owner, id)
	if err != nil {
		return err
	}
	if snap.Revision != expected {
		return chatstore.ErrConflict
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return fmt.Errorf("set EDITOR or --editor to edit synced chats")
	}
	dir, err := os.MkdirTemp("", "serverside-chat-edit-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "conversation.json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(append([]byte(nil), snap.Conversation...), '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}

	cmd := exec.Command(parts[0], append(parts[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("chat editor exited without saving: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > chatstore.MaxConversationBytes {
		return fmt.Errorf("unsafe edited chat file")
	}
	f, err = os.Open(path)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(f, chatstore.MaxConversationBytes+1))
	closeErr = f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if len(raw) > chatstore.MaxConversationBytes || chatstore.Validate(id, raw) != nil {
		return fmt.Errorf("invalid edited conversation")
	}
	if chatstore.Equal(snap.Conversation, raw) {
		fmt.Println("Chat unchanged")
		return nil
	}
	if dryRun {
		fmt.Println("Edited conversation is valid (dry run; nothing saved)")
		return nil
	}
	if !approved {
		return fmt.Errorf("edited conversation not saved; pass --yes to approve a revision-checked update")
	}
	opID, err := node.RandomToken(18)
	if err != nil {
		return err
	}
	unlock, err := store.TryJobLease(id)
	if err != nil {
		return err
	}
	defer unlock()
	result, err := store.SaveCompared(id, owner, opID, expected, snap.Conversation, raw)
	if err != nil {
		return err
	}
	fmt.Printf("Saved chat revision %d\n", result.Revision)
	return nil
}
