package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nwverse/personal-agent-club/internal/protocol"
)

var ctx = context.Background()

// each runs a test against both stores.
func each(t *testing.T, test func(t *testing.T, s Store)) {
	t.Run("memory", func(t *testing.T) { test(t, NewMemory()) })
	t.Run("sqlite", func(t *testing.T) {
		s, err := OpenSQLite(filepath.Join(t.TempDir(), "club.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		test(t, s)
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const (
	acct = "a000000000001"
	key  = "agent:cue:a000000000001:main"
	sub  = "agent:cue:a000000000001:sub"
	now  = int64(1_761_900_000_000)
)

func text(id, role, t string, at int64) Message {
	return Message{ExternalID: id, Role: role, Blocks: []protocol.Block{protocol.TextBlock(t)}, CreateTimeMs: at}
}

func seed(t *testing.T, s Store) {
	t.Helper()
	must(t, s.InsertAccount(ctx, Account{AccountID: acct, Provider: "cue", DisplayName: "Cue", Status: protocol.AccountActive,
		ExternalAccountID: "manus", AssistantControl: "off", CreatedAt: now, UpdatedAt: now}))
	_, err := s.UpsertSessions(ctx, []Session{
		{SessionKey: key, AccountID: acct, Provider: "cue", ExternalID: "main", Title: "Cue", Kind: protocol.KindMain,
			CanSend: true, Ref: map[string]string{"taskId": "main"}, AvatarURL: "https://example.com/a.png", RankTimeMs: now - 60_000},
		{SessionKey: sub, AccountID: acct, Provider: "cue", ExternalID: "sub", Title: "订机票", Kind: protocol.KindTask,
			ParentSessionKey: key, Ref: map[string]string{"taskId": "sub"}, RankTimeMs: now - 120_000},
	}, now)
	must(t, err)
	_, err = s.UpsertMessages(ctx, key, []Message{text("m1", "user", "查航班", now-90_000), text("m2", "assistant", "找到 3 个", now-60_000)}, now)
	must(t, err)
}

func session(t *testing.T, s Store, k string) Session {
	t.Helper()
	x, ok, err := s.Session(ctx, k)
	if err != nil || !ok {
		t.Fatalf("session %s: %v %v", k, ok, err)
	}
	return x
}

func TestAccounts(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		seed(t, s)
		must(t, s.InsertAccount(ctx, Account{AccountID: "a000000000002", Provider: "groq", DisplayName: "Groq",
			Status: protocol.AccountActive, CreatedAt: now + 1, UpdatedAt: now + 1}))
		list, err := s.ListAccounts(ctx)
		must(t, err)
		if len(list) != 2 || list[0].AccountID != acct || list[1].AssistantControl != "off" && list[1].AssistantControl != "" {
			t.Fatalf("%+v", list)
		}
		errText, paused, show, control, synced := "AUTH_FAILED", protocol.AccountPaused, true, "read", int64(5)
		must(t, s.UpdateAccount(ctx, acct, AccountPatch{LastError: &errText, Status: &paused, ShowQuota: &show,
			AssistantControl: &control, LastSyncedAt: &synced}, now+5))
		a, ok, err := s.Account(ctx, acct)
		must(t, err)
		want := Account{AccountID: acct, Provider: "cue", DisplayName: "Cue", Status: paused, ExternalAccountID: "manus",
			LastSyncedAt: 5, LastError: "AUTH_FAILED", AssistantControl: "read", ShowQuota: true, CreatedAt: now, UpdatedAt: now + 5}
		if !ok || a != want {
			t.Fatalf("%+v", a)
		}
		empty := ""
		active := protocol.AccountActive
		must(t, s.UpdateAccount(ctx, acct, AccountPatch{Status: &active, PreservePaused: true, LastError: &empty}, now+6))
		if a, _, _ := s.Account(ctx, acct); a.Status != paused || a.LastError != "" {
			t.Fatalf("%+v", a)
		}
		if err := s.UpdateAccount(ctx, "a0000000000ff", AccountPatch{}, now); err != ErrNotFound {
			t.Fatal(err)
		}

		must(t, s.RemoveAccount(ctx, acct, false, now+7))
		if _, ok, _ := s.Account(ctx, acct); ok {
			t.Fatal("a removed account is listed")
		}
		if _, ok, _ := s.Session(ctx, key); ok {
			t.Fatal("a removed account's session is listed")
		}
		if list, _ := s.Sessions(ctx, 0); len(list) != 0 {
			t.Fatalf("%+v", list)
		}
		if c, _ := s.Counts(ctx, acct); c != (Counts{Sessions: 2, Messages: 2, LatestMessageMs: now - 60_000}) {
			t.Fatalf("kept data: %+v", c)
		}
		must(t, s.RemoveAccount(ctx, acct, true, now+8))
		if c, _ := s.Counts(ctx, acct); c != (Counts{}) {
			t.Fatalf("deleted data: %+v", c)
		}
		if err := s.RemoveAccount(ctx, acct, true, now); err != ErrNotFound {
			t.Fatal(err)
		}
	})
}

func TestSessionsStartReadAndCountChanges(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		seed(t, s)
		main := session(t, s, key)
		if main.ReadTimeMs != now || main.UnreadCount != 0 || main.AvatarURL != "https://example.com/a.png" ||
			!reflect.DeepEqual(main.Ref, map[string]string{"taskId": "main"}) || !main.CanSend || main.PreviewText != "找到 3 个" ||
			main.PreviewIsMine || main.RankTimeMs != now-60_000 {
			t.Fatalf("%+v", main)
		}
		if x := session(t, s, sub); x.ParentSessionKey != key || x.CanSend || x.Kind != protocol.KindTask {
			t.Fatalf("%+v", x)
		}
		same := []Session{{SessionKey: key, AccountID: acct, Provider: "cue", ExternalID: "main", Title: "Cue",
			Kind: protocol.KindMain, CanSend: true, Ref: map[string]string{"taskId": "main"}, AvatarURL: "https://example.com/a.png",
			RankTimeMs: now - 70_000}}
		n, err := s.UpsertSessions(ctx, same, now+1)
		must(t, err)
		if n != 0 {
			t.Fatalf("%d changed", n)
		}
		same[0].Title = "Cue 2"
		if n, _ := s.UpsertSessions(ctx, same, now+2); n != 1 {
			t.Fatalf("%d changed", n)
		}
		if x := session(t, s, key); x.Title != "Cue 2" || x.RankTimeMs != now-60_000 || x.UpdatedAt != now+2 {
			t.Fatalf("%+v", x)
		}
	})
}

func TestMessagesUnreadAndPreview(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		seed(t, s)
		n, err := s.UpsertMessages(ctx, key, []Message{text("m1", "user", "查航班", now-90_000), text("m2", "assistant", "找到 3 个", now-60_000)}, now+1)
		must(t, err)
		if n != 0 {
			t.Fatalf("%d changed", n)
		}
		at := now + 60_000
		must2(t, s, key, []Message{text("m3", "assistant", "要订哪一个？", at)}, at)
		if x := session(t, s, key); x.UnreadCount != 1 || x.RankTimeMs != at || x.PreviewText != "要订哪一个？" {
			t.Fatalf("replies after the first sync are unread: %+v", x)
		}
		must(t, s.MarkRead(ctx, key, at+1))
		if x := session(t, s, key); x.UnreadCount != 0 || x.ReadTimeMs != at+1 {
			t.Fatalf("%+v", x)
		}
		at += 60_000
		must2(t, s, key, []Message{text("m4", "assistant", "还在吗", at), text("m5", "user", "第一个", at+1000)}, at)
		if x := session(t, s, key); x.UnreadCount != 0 || !x.PreviewIsMine || x.PreviewText != "第一个" {
			t.Fatalf("replies before my own message are read: %+v", x)
		}
		must2(t, s, key, []Message{{ExternalID: "m6", Role: "tool", Blocks: []protocol.Block{{Type: "tool-call", Name: "x"}}, CreateTimeMs: at + 2000},
			text("m7", "assistant", "好的", at+3000)}, at+3000)
		if x := session(t, s, key); x.UnreadCount != 1 || x.PreviewText != "好的" {
			t.Fatalf("%+v", x)
		}
		must2(t, s, key, []Message{{ExternalID: "late", Role: "assistant", Blocks: []protocol.Block{protocol.TextBlock("无时间")}}}, at+9000)
		page, _, err := s.Messages(ctx, key, 1, nil)
		must(t, err)
		if page[0].ExternalID != "late" || page[0].CreateTimeMs != at+9000 {
			t.Fatalf("a message without time keeps its first-seen time: %+v", page)
		}
		must2(t, s, key, []Message{{ExternalID: "late", Role: "assistant", Blocks: []protocol.Block{protocol.TextBlock("无时间!")}}}, at+20_000)
		page, _, _ = s.Messages(ctx, key, 1, nil)
		if page[0].CreateTimeMs != at+9000 || page[0].Blocks[0].Text != "无时间!" {
			t.Fatalf("%+v", page)
		}
	})
}

func must2(t *testing.T, s Store, k string, msgs []Message, at int64) {
	t.Helper()
	if _, err := s.UpsertMessages(ctx, k, msgs, at); err != nil {
		t.Fatal(err)
	}
}

func TestMessagesPageBackwards(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		seed(t, s)
		var msgs []Message
		for i := range 7 {
			m := text(fmt.Sprintf("p%d", i), "assistant", fmt.Sprint(i), now+int64(i/2))
			m.Position = int64(i % 2)
			m.Blocks = append(m.Blocks, protocol.Block{Type: "future", Raw: []byte(`{"type":"future","x":1}`)})
			msgs = append(msgs, m)
		}
		must2(t, s, sub, msgs, now)
		var got []string
		var before *Cursor
		for {
			page, more, err := s.Messages(ctx, sub, 3, before)
			must(t, err)
			var ids []string
			for _, m := range page {
				ids = append(ids, m.ExternalID)
			}
			got = append(ids, got...)
			if !more {
				break
			}
			before = &Cursor{CreateTimeMs: page[0].CreateTimeMs, Position: page[0].Position, ExternalID: page[0].ExternalID}
		}
		if !reflect.DeepEqual(got, []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6"}) {
			t.Fatal(got)
		}
		page, _, _ := s.Messages(ctx, sub, 1, nil)
		if string(page[0].Blocks[1].Raw) != `{"type":"future","x":1}` {
			t.Fatalf("unknown blocks are kept verbatim: %+v", page[0].Blocks)
		}
	})
}

func TestFeedOrderPinsAndHidden(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		seed(t, s)
		rows := []Session{}
		for i, k := range []string{"x1", "x2", "x3", "x4"} {
			rows = append(rows, Session{SessionKey: "agent:cue:" + acct + ":" + k, AccountID: acct, Provider: "cue", ExternalID: k,
				Title: k, Kind: protocol.KindMain, RankTimeMs: now + int64(i), Ref: map[string]string{}, UpdatedAt: now})
		}
		rows[0].OnTopRankTimeMs = 5
		rows[1].OnTopRankTimeMs = 9
		rows[3].IsHidden = true
		must(t, s.Import(ctx, Import{Sessions: rows}))
		list, err := s.Sessions(ctx, 0)
		must(t, err)
		var got []string
		for _, x := range list {
			got = append(got, x.ExternalID)
		}
		if !reflect.DeepEqual(got, []string{"x2", "x1", "x3", "main", "sub"}) {
			t.Fatal(got)
		}
		if list, _ := s.Sessions(ctx, 2); len(list) != 2 {
			t.Fatal(len(list))
		}
		all, _ := s.AccountSessions(ctx, acct)
		if len(all) != 6 || all[0].ExternalID != "x4" {
			t.Fatalf("%+v", all)
		}
	})
}

func TestOutbox(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		seed(t, s)
		o := Outbox{ClientID: "c1", SessionKey: key, AccountID: acct, Text: "hello", CreatedAt: now}
		row, inserted, err := s.EnqueueOutbox(ctx, o)
		must(t, err)
		if !inserted || row.Status != protocol.LocalPending || row.UpdatedAt != now {
			t.Fatalf("%+v", row)
		}
		again, inserted, _ := s.EnqueueOutbox(ctx, Outbox{ClientID: "c1", SessionKey: sub, Text: "other", CreatedAt: now + 1})
		if inserted || again.SessionKey != key || again.Text != "hello" {
			t.Fatalf("deduplicated by client id: %+v", again)
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		claims := 0
		for range 8 {
			wg.Go(func() {
				if _, ok, err := s.ClaimOutbox(ctx, "c1", now+1); err == nil && ok {
					mu.Lock()
					claims++
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		if claims != 1 {
			t.Fatalf("claimed %d times", claims)
		}
		cur, _, _ := s.Outbox(ctx, "c1")
		if cur.Status != protocol.LocalClaimed || cur.AttemptCount != 1 {
			t.Fatalf("%+v", cur)
		}
		if ok, _ := s.CancelOutbox(ctx, "c1", key, now); ok {
			t.Fatal("cancelled a claimed send")
		}
		abandoned, err := s.AbandonClaimedOutbox(ctx, now+2)
		must(t, err)
		if len(abandoned) != 1 || abandoned[0].Status != protocol.LocalUnconfirmed || abandoned[0].LastError != "SEND_UNCONFIRMED" {
			t.Fatalf("%+v", abandoned)
		}
		if _, ok, _ := s.ClaimOutbox(ctx, "c1", now+3); ok {
			t.Fatal("an unconfirmed send was claimed again")
		}
		if pending, _ := s.PendingOutbox(ctx); len(pending) != 0 {
			t.Fatalf("%+v", pending)
		}
		if ok, _ := s.ConfirmOutbox(ctx, "c1", protocol.LocalSent, "", now+4); !ok {
			t.Fatal("confirm")
		}
		if ok, _ := s.ConfirmOutbox(ctx, "c1", protocol.LocalFailed, "X", now+5); ok {
			t.Fatal("confirmed a send that was not unconfirmed")
		}

		_, _, _ = s.EnqueueOutbox(ctx, Outbox{ClientID: "c2", SessionKey: key, AccountID: acct, Text: "two", CreatedAt: now + 10})
		_, _, _ = s.EnqueueOutbox(ctx, Outbox{ClientID: "c3", SessionKey: key, AccountID: acct, Text: "three", CreatedAt: now + 5})
		pending, _ := s.PendingOutbox(ctx)
		if len(pending) != 2 || pending[0].ClientID != "c3" {
			t.Fatalf("%+v", pending)
		}
		if ok, _ := s.CancelOutbox(ctx, "c2", sub, now); ok {
			t.Fatal("cancelled in another session")
		}
		if ok, _ := s.CancelOutbox(ctx, "c2", key, now+11); !ok {
			t.Fatal("cancel")
		}
		if ok, _ := s.CancelOutbox(ctx, "c2", key, now+12); ok {
			t.Fatal("cancelled twice")
		}
		if c2, _, _ := s.Outbox(ctx, "c2"); c2.Status != protocol.LocalFailed || c2.LastError != "CANCELLED" {
			t.Fatalf("%+v", c2)
		}
		if ok, _ := s.DismissOutbox(ctx, "c3", key); ok {
			t.Fatal("dismissed a pending send")
		}
		if ok, _ := s.DismissOutbox(ctx, "c2", key); !ok {
			t.Fatal("dismiss")
		}
		if _, ok, _ := s.Outbox(ctx, "c2"); ok {
			t.Fatal("a dismissed send is kept")
		}
		if _, inserted, _ := s.EnqueueOutbox(ctx, Outbox{ClientID: "c2", SessionKey: key, AccountID: acct, Text: "again", CreatedAt: now + 20}); !inserted {
			t.Fatal("a dismissed client id is not released")
		}
		must(t, s.SettleOutbox(ctx, "c3", protocol.LocalFailed, "INVALID_ARGUMENT", now+21))
		if c3, _, _ := s.Outbox(ctx, "c3"); c3.Status != protocol.LocalFailed || c3.LastError != "INVALID_ARGUMENT" || c3.UpdatedAt != now+21 {
			t.Fatalf("%+v", c3)
		}
		if err := s.SettleOutbox(ctx, "nope", protocol.LocalSent, "", now); err != ErrNotFound {
			t.Fatal(err)
		}
	})
}

func TestLocalMessagesUntilTheAgentEchoesThem(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		seed(t, s)
		for i, o := range []Outbox{{ClientID: "sent-old", Text: "旧"}, {ClientID: "sent-new", Text: " 第二个 "}, {ClientID: "pending", Text: "等"}} {
			o.SessionKey, o.AccountID, o.CreatedAt = key, acct, now+int64(i)
			_, _, err := s.EnqueueOutbox(ctx, o)
			must(t, err)
		}
		must(t, s.SettleOutbox(ctx, "sent-old", protocol.LocalSent, "", now))
		must(t, s.SettleOutbox(ctx, "sent-new", protocol.LocalSent, "", now+100))
		ids := func() []string {
			rows, err := s.LocalMessages(ctx, key, now+50)
			must(t, err)
			var out []string
			for _, o := range rows {
				out = append(out, o.ClientID)
			}
			return out
		}
		if got := ids(); !reflect.DeepEqual(got, []string{"sent-new", "pending"}) {
			t.Fatal(got)
		}
		must2(t, s, key, []Message{text("echo", "user", "第二个", now+5_000)}, now+5_000)
		if got := ids(); !reflect.DeepEqual(got, []string{"pending"}) {
			t.Fatal(got)
		}
		if keys, _ := s.ActiveSessionKeys(ctx, acct, now); !reflect.DeepEqual(keys, []string{key}) {
			t.Fatal(keys)
		}
		if keys, _ := s.ActiveSessionKeys(ctx, acct, now+10_000); len(keys) != 0 {
			t.Fatal(keys)
		}
	})
}

func hitIDs(t *testing.T, s Store, q string) []string {
	t.Helper()
	hits, err := s.Search(ctx, q, 20)
	must(t, err)
	out := []string{}
	for _, h := range hits {
		out = append(out, h.ExternalID)
	}
	return out
}

func TestSearch(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		seed(t, s)
		seedRand := rand.New(rand.NewPCG(42, 42))
		var b strings.Builder
		b.WriteString("开头检索词")
		for range 30_000 {
			b.WriteRune(rune(0x4e00 + seedRand.IntN(20_000)))
		}
		report := b.String()
		must2(t, s, key, []Message{
			{ExternalID: "long-report", Role: "assistant", Blocks: []protocol.Block{protocol.TextBlock(report)}, CreateTimeMs: now - 50_000},
			text("deploy", "assistant", "redeploy finished", now-40_000),
			text("mixed", "assistant", "Deploy 完成了，明早 9 点提醒你开会。", now-30_000),
			{ExternalID: "file", Role: "assistant", Blocks: []protocol.Block{{Type: "attachment", Name: "flights.md"}}, CreateTimeMs: now - 20_000},
			{ExternalID: "plan", Role: "tool", Blocks: []protocol.Block{{Type: "tool-result", Name: "plan", Text: "✓ 搜索航班"}}, CreateTimeMs: now - 10_000},
		}, now)
		page, _, _ := s.Messages(ctx, key, 1, &Cursor{CreateTimeMs: now - 40_000})
		if page[0].ExternalID != "long-report" || page[0].Blocks[0].Text != report {
			t.Fatal("the long report is kept whole")
		}
		for q, want := range map[string][]string{
			"航班":        {"plan", "m1"},
			"查航班":       {"m1"},
			"开头检索词":     {"long-report"},
			"提醒":        {"mixed"},
			"提醒你开会":     {"mixed"},
			"deploy":    {"mixed", "deploy"},
			"DEPLOY 完成": {"mixed"},
			"finished":  {"deploy"},
			"flights":   {"file"},
			"missing":   {},
			"%":         {},
			"_":         {},
			"  ":        {},
			"找到 3":      {"m2"},
		} {
			if got := hitIDs(t, s, q); !reflect.DeepEqual(got, want) {
				t.Errorf("%q: %v, want %v", q, got, want)
			}
		}
		hits, _ := s.Search(ctx, "航班", 1)
		if len(hits) != 1 || hits[0] != (protocol.SearchHit{SessionKey: key, ExternalID: "plan", Text: "✓ 搜索航班", Role: "tool",
			CreateTimeMs: now - 10_000, Title: "Cue", Provider: "cue"}) {
			t.Fatalf("%+v", hits)
		}
		must(t, s.RemoveAccount(ctx, acct, false, now))
		if got := hitIDs(t, s, "航班"); len(got) != 0 {
			t.Fatalf("a removed account's messages are found: %v", got)
		}
	})
}

func TestSettingsAndImport(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		if _, ok, _ := s.Setting(ctx, "browserIdentity"); ok {
			t.Fatal("unset")
		}
		must(t, s.PutSetting(ctx, "browserIdentity", `{"locale":"zh-CN"}`))
		must(t, s.PutSetting(ctx, "browserIdentity", `{"locale":"en"}`))
		if v, ok, _ := s.Setting(ctx, "browserIdentity"); !ok || v != `{"locale":"en"}` {
			t.Fatal(v)
		}

		rows := Import{
			Accounts: []Account{{AccountID: acct, Provider: "cue", DisplayName: "Cue", Status: protocol.AccountError, LastError: "AUTH_FAILED",
				ExternalAccountID: "manus", LastSyncedAt: now, AssistantControl: "off", CreatedAt: 1, UpdatedAt: 2}},
			Sessions: []Session{{SessionKey: key, AccountID: acct, Provider: "cue", ExternalID: "main", Title: "Cue", Kind: "main",
				CanSend: true, Ref: map[string]string{"taskId": "main"}, AvatarURL: "data:image/png;base64,AA==", RankTimeMs: now, OnTopRankTimeMs: 3,
				ReadTimeMs: now - 5, UnreadCount: 2, PreviewText: "hi", UpdatedAt: now}},
			Messages: []Message{{SessionKey: key, ExternalID: "m1", Role: "assistant", Blocks: []protocol.Block{protocol.TextBlock("你好航班")},
				CreateTimeMs: now - 1, Position: 7}},
			Outbox: []Outbox{{ClientID: "c1", SessionKey: key, AccountID: acct, Text: "x", Status: protocol.LocalUnconfirmed,
				LastError: "SEND_UNCONFIRMED", AttemptCount: 1, CreatedAt: now - 3, UpdatedAt: now - 2}},
		}
		must(t, s.Import(ctx, rows))
		rows.Outbox[0].Status = protocol.LocalPending
		must(t, s.Import(ctx, rows))
		a, _, _ := s.Account(ctx, acct)
		if a != rows.Accounts[0] {
			t.Fatalf("%+v", a)
		}
		x := session(t, s, key)
		want := rows.Sessions[0]
		if !reflect.DeepEqual(x, want) {
			t.Fatalf("%+v\n%+v", x, want)
		}
		page, _, _ := s.Messages(ctx, key, 10, nil)
		if len(page) != 1 || page[0].Position != 7 || page[0].CreateTimeMs != now-1 {
			t.Fatalf("%+v", page)
		}
		if o, _, _ := s.Outbox(ctx, "c1"); o.Status != protocol.LocalUnconfirmed || o.AttemptCount != 1 || o.CreatedAt != now-3 {
			t.Fatalf("an imported send already there is left alone: %+v", o)
		}
		if got := hitIDs(t, s, "航班"); !slices.Equal(got, []string{"m1"}) {
			t.Fatal(got)
		}
		if c, _ := s.Counts(ctx, acct); c != (Counts{Sessions: 1, Messages: 1, LatestMessageMs: now - 1}) {
			t.Fatalf("%+v", c)
		}
	})
}

func TestSQLiteKeepsItsDataInOneFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spaces", "default", "club.db")
	s, err := OpenSQLite(path)
	must(t, err)
	seed(t, s)
	var mode string
	must(t, s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode))
	if mode != "wal" {
		t.Fatal(mode)
	}
	must(t, s.Close())
	fi, err := os.Stat(path)
	must(t, err)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v", fi.Mode())
	}
	if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o700 {
		t.Fatalf("%v", di.Mode())
	}
	s, err = OpenSQLite(path)
	must(t, err)
	defer s.Close()
	if x := session(t, s, key); x.PreviewText != "找到 3 个" {
		t.Fatalf("%+v", x)
	}
	if got := hitIDs(t, s, "航班"); !slices.Equal(got, []string{"m1"}) {
		t.Fatal(got)
	}
	_, err = s.db.Exec(`PRAGMA user_version = 99`)
	must(t, err)
	must(t, s.Close())
	if _, err := OpenSQLite(path); err == nil || !strings.Contains(err.Error(), "schema version 99") {
		t.Fatal(err)
	}
}

func TestCJKTokens(t *testing.T) {
	if got := CJKTokens("Ａ明早9点"); got != "明早 早点 明早点" {
		t.Fatal(got)
	}
	if q := parseQuery("提醒你开会"); !slices.Equal(q.cjk, []string{"提醒你", "醒你开", "你开会"}) || q.fts() != `("提醒你开会") OR ("提醒你" AND "醒你开" AND "你开会")` {
		t.Fatalf("%+v %s", q, q.fts())
	}
	if q := parseQuery(`a"b`); q.fts() != `("a" AND "b")` {
		t.Fatal(q.fts())
	}
	if got := BlocksText([]protocol.Block{protocol.TextBlock("a"), {Type: "tool-call", Name: "x"}, {Type: "attachment", Name: "f.md"},
		{Type: "error", Text: "e"}, {Type: "tool-result", Text: "r"}}); got != "a\nf.md\ne\nr" {
		t.Fatal(got)
	}
}
