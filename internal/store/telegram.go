package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

func jsonText(value json.RawMessage) string {
	if len(value) == 0 {
		return "null"
	}
	return string(value)
}

func (s *Store) migrateTelegram() error {
	for _, col := range []struct{ name, definition string }{
		{"reply_markup", "TEXT NOT NULL DEFAULT 'null'"},
		{"rich_message", "TEXT NOT NULL DEFAULT 'null'"},
		{"thread_id", "INTEGER NOT NULL DEFAULT 0"},
		{"media_group_id", "TEXT NOT NULL DEFAULT ''"},
		{"callback_id", "TEXT NOT NULL DEFAULT ''"},
	} {
		var exists int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name=?", col.name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if _, err := s.db.Exec("ALTER TABLE messages ADD COLUMN " + col.name + " " + col.definition); err != nil {
				return err
			}
		}
	}
	_, err := s.db.Exec(`
 CREATE TABLE IF NOT EXISTS topics (bot_id INTEGER,chat_id INTEGER,thread_id INTEGER,name TEXT,closed INTEGER DEFAULT 0,PRIMARY KEY(bot_id,chat_id,thread_id));
 CREATE TABLE IF NOT EXISTS callback_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, bot_id INTEGER NOT NULL, callback_id TEXT NOT NULL,
  chat_id INTEGER NOT NULL, message_id INTEGER NOT NULL, inline_message_id TEXT NOT NULL,
  from_id INTEGER NOT NULL, from_user TEXT NOT NULL, data TEXT NOT NULL, game_short_name TEXT NOT NULL,
  date INTEGER NOT NULL, answered INTEGER NOT NULL DEFAULT 0, UNIQUE(bot_id,callback_id)
 );
 CREATE TABLE IF NOT EXISTS inline_keyboards(bot_id INTEGER NOT NULL,inline_message_id TEXT NOT NULL,reply_markup TEXT NOT NULL,PRIMARY KEY(bot_id,inline_message_id));
 CREATE TABLE IF NOT EXISTS handled_updates(bot_id INTEGER NOT NULL,update_id INTEGER NOT NULL,PRIMARY KEY(bot_id,update_id));
 CREATE TABLE IF NOT EXISTS pending_updates (
  bot_id INTEGER NOT NULL, update_id INTEGER NOT NULL, data TEXT NOT NULL,
  PRIMARY KEY(bot_id,update_id)
 );
 CREATE TABLE IF NOT EXISTS update_consumers (
  bot_id INTEGER PRIMARY KEY, confirmed_offset INTEGER NOT NULL DEFAULT 0, allowed_updates TEXT NOT NULL DEFAULT 'null'
 );`)
	if err != nil {
		return err
	}
	var hasAnswer int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('callback_events') WHERE name='answered'").Scan(&hasAnswer); err != nil {
		return err
	}
	if hasAnswer == 0 {
		_, err = s.db.Exec("ALTER TABLE callback_events ADD COLUMN answered INTEGER NOT NULL DEFAULT 0")
	}
	return err
}

// SaveCallbackEvent uses Telegram's callback id as the idempotency key.
func (s *Store) SaveCallbackEvent(e models.CallbackEvent) (int64, bool, error) {
	res, err := s.db.Exec(`INSERT INTO callback_events(bot_id,callback_id,chat_id,message_id,inline_message_id,from_id,from_user,data,game_short_name,date)
 VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(bot_id,callback_id) DO NOTHING`, e.BotID, e.CallbackID, e.ChatID, e.MessageID, e.InlineMessageID, e.FromID, e.FromUser, e.Data, e.GameShortName, e.Date)
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	var id int64
	err = s.db.QueryRow("SELECT id FROM callback_events WHERE bot_id=? AND callback_id=?", e.BotID, e.CallbackID).Scan(&id)
	return id, n > 0, err
}

func (s *Store) GetCallbackEvents(botID, chatID int64, limit int) ([]models.CallbackEvent, error) {
	limit = min(max(limit, 1), 100)
	rows, err := s.db.Query(`SELECT id,bot_id,callback_id,chat_id,message_id,inline_message_id,from_id,from_user,data,game_short_name,date,answered FROM callback_events WHERE bot_id=? AND (?=0 OR chat_id=?) ORDER BY id DESC LIMIT ?`, botID, chatID, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]models.CallbackEvent, 0)
	for rows.Next() {
		var e models.CallbackEvent
		if err := rows.Scan(&e.ID, &e.BotID, &e.CallbackID, &e.ChatID, &e.MessageID, &e.InlineMessageID, &e.FromID, &e.FromUser, &e.Data, &e.GameShortName, &e.Date, &e.Answered); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range events {
		if events[i].InlineMessageID != "" {
			markup, err := s.InlineKeyboard(botID, events[i].InlineMessageID)
			if err != nil {
				return nil, err
			}
			events[i].ReplyMarkup = markup
		}
	}
	return events, nil
}

func (s *Store) UpdateReplyMarkup(botID, chatID int64, msgID int, markup json.RawMessage) error {
	_, err := s.db.Exec("UPDATE messages SET reply_markup=? WHERE bot_id=? AND chat_id=? AND id=? AND deleted=0", jsonText(markup), botID, chatID, msgID)
	if err == nil {
		if m, e := s.GetMessage(botID, chatID, msgID); e == nil {
			s.notifySubscribers(*m)
		}
	}
	return err
}

func (s *Store) EnqueueTelegramUpdate(botID int64, update map[string]any) error {
	id, ok := update["update_id"].(float64)
	if !ok {
		return fmt.Errorf("update_id must be a number")
	}
	data, err := json.Marshal(update)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO pending_updates(bot_id,update_id,data) SELECT ?,?,? WHERE ?>=COALESCE((SELECT confirmed_offset FROM update_consumers WHERE bot_id=?),0) ON CONFLICT(bot_id,update_id) DO NOTHING`, botID, int64(id), string(data), int64(id), botID)
	return err
}

// PendingTelegramUpdates confirms and reads in one transaction. Offsets are durable.
func (s *Store) PendingTelegramUpdates(botID, offset int64, limit int) ([]models.QueuedUpdate, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if offset < 0 {
		var earliest sql.NullInt64
		if err := tx.QueryRow(`SELECT MIN(update_id) FROM (SELECT update_id FROM pending_updates WHERE bot_id=? ORDER BY update_id DESC LIMIT ?)`, botID, -offset).Scan(&earliest); err != nil {
			return nil, err
		}
		offset = earliest.Int64
	}

	if offset > 0 {
		if _, err = tx.Exec(`INSERT INTO update_consumers(bot_id,confirmed_offset) VALUES(?,?) ON CONFLICT(bot_id) DO UPDATE SET confirmed_offset=MAX(confirmed_offset,excluded.confirmed_offset)`, botID, offset); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("DELETE FROM pending_updates WHERE bot_id=? AND update_id<?", botID, offset); err != nil {
			return nil, err
		}
	}
	var filterJSON string
	err = tx.QueryRow("SELECT allowed_updates FROM update_consumers WHERE bot_id=?", botID).Scan(&filterJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var allowed []string
	if filterJSON != "" {
		if err = json.Unmarshal([]byte(filterJSON), &allowed); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query("SELECT update_id,data FROM pending_updates WHERE bot_id=? AND update_id>=? ORDER BY update_id", botID, max(offset, 0))
	if err != nil {
		return nil, err
	}
	updates := make([]models.QueuedUpdate, 0)
	for rows.Next() {
		var u models.QueuedUpdate
		var data string
		if err = rows.Scan(&u.UpdateID, &data); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal([]byte(data), &u.Data); err != nil {
			rows.Close()
			return nil, err
		}
		match := allowed == nil
		for _, key := range allowed {
			if _, ok := u.Data[key]; ok {
				match = true
				break
			}
		}
		if match {
			updates = append(updates, u)
			if len(updates) >= limit {
				break
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return updates, nil
}

func (s *Store) SetAllowedUpdates(botID int64, allowed []string) error {
	data, err := json.Marshal(allowed)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO update_consumers(bot_id,allowed_updates) VALUES(?,?) ON CONFLICT(bot_id) DO UPDATE SET allowed_updates=excluded.allowed_updates`, botID, string(data))
	return err
}
func (s *Store) PendingUpdateCount(botID int64) (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM pending_updates WHERE bot_id=?", botID).Scan(&n)
	return n, err
}
func (s *Store) ClearPendingUpdates(botID int64) error {
	_, err := s.db.Exec("DELETE FROM pending_updates WHERE bot_id=?", botID)
	return err
}

// Topic metadata is updated from service messages and successful API calls.
func (s *Store) SaveTopic(botID, chatID int64, threadID int, name string) error {
	_, err := s.db.Exec(`INSERT INTO topics(bot_id,chat_id,thread_id,name) VALUES(?,?,?,?) ON CONFLICT(bot_id,chat_id,thread_id) DO UPDATE SET name=CASE WHEN excluded.name!='' THEN excluded.name ELSE topics.name END`, botID, chatID, threadID, name)
	return err
}
func (s *Store) GetTopics(botID, chatID int64) ([]map[string]any, error) {
	rows, err := s.db.Query("SELECT thread_id,name,closed FROM topics WHERE bot_id=? AND chat_id=? ORDER BY thread_id", botID, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]map[string]any, 0)
	for rows.Next() {
		var id int
		var name string
		var closed bool
		if err := rows.Scan(&id, &name, &closed); err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"message_thread_id": id, "name": name, "closed": closed})
	}
	return result, rows.Err()
}

func (s *Store) SetTopicClosed(botID, chatID int64, threadID int, closed bool) error {
	_, err := s.db.Exec("UPDATE topics SET closed=? WHERE bot_id=? AND chat_id=? AND thread_id=?", closed, botID, chatID, threadID)
	return err
}

type rawJSONScanner struct{ target *json.RawMessage }

func jsonScan(target *json.RawMessage) *rawJSONScanner { return &rawJSONScanner{target: target} }
func (s *rawJSONScanner) Scan(value any) error {
	switch v := value.(type) {
	case string:
		*s.target = json.RawMessage(v)
	case []byte:
		*s.target = append((*s.target)[:0], v...)
	case nil:
		*s.target = nil
	default:
		return fmt.Errorf("unsupported JSON column %T", value)
	}
	return nil
}

func (s *Store) AllowsTelegramUpdate(botID int64, update map[string]any) (bool, error) {
	var data string
	err := s.db.QueryRow("SELECT allowed_updates FROM update_consumers WHERE bot_id=?", botID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var keys []string
	if err := json.Unmarshal([]byte(data), &keys); err != nil {
		return false, err
	}
	if keys == nil {
		return true, nil
	}
	for _, key := range keys {
		if _, ok := update[key]; ok {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) GetMessagesByThread(botID, chatID int64, threadID, limit, offset int) ([]models.Message, error) {
	rows, err := s.db.Query(`SELECT id,bot_id,chat_id,from_user,from_id,text,rich_text,date,reply_to_id,deleted,media_type,file_id,from_is_bot,sender_tag,reply_markup,rich_message,thread_id,media_group_id,callback_id FROM messages WHERE bot_id=? AND chat_id=? AND thread_id=? ORDER BY date DESC LIMIT ? OFFSET ?`, botID, chatID, threadID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]models.Message, 0)
	for rows.Next() {
		var m models.Message
		if err := rows.Scan(&m.ID, &m.BotID, &m.ChatID, &m.FromUser, &m.FromID, &m.Text, &m.RichText, &m.Date, &m.ReplyToID, &m.Deleted, &m.MediaType, &m.FileID, &m.FromIsBot, &m.SenderTag, jsonScan(&m.ReplyMarkup), jsonScan(&m.RichMessage), &m.ThreadID, &m.MediaGroupID, &m.CallbackID); err != nil {
			return nil, err
		}
		m.DateStr = time.UnixMilli(m.Date).Format("2006-01-02 15:04:05")
		result = append(result, m)
	}
	return result, rows.Err()
}

func (s *Store) MarkCallbackAnswered(botID int64, id string) error {
	_, err := s.db.Exec("UPDATE callback_events SET answered=1 WHERE bot_id=? AND callback_id=?", botID, id)
	return err
}
func (s *Store) HasInlineCallback(botID int64, id string) bool {
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM callback_events WHERE bot_id=? AND inline_message_id=?", botID, id).Scan(&n)
	return n > 0
}

func (s *Store) HandledTelegramUpdate(botID, updateID int64) (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM handled_updates WHERE bot_id=? AND update_id=?", botID, updateID).Scan(&n)
	return n > 0, err
}
func (s *Store) MarkTelegramUpdateHandled(botID, updateID int64) error {
	_, err := s.db.Exec("INSERT INTO handled_updates(bot_id,update_id) VALUES(?,?) ON CONFLICT DO NOTHING", botID, updateID)
	return err
}
func (s *Store) HasCallback(botID int64, id string) (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM callback_events WHERE bot_id=? AND callback_id=?", botID, id).Scan(&n)
	return n > 0, err
}

func (s *Store) SaveInlineKeyboard(botID int64, id string, markup json.RawMessage) error {
	_, err := s.db.Exec(`INSERT INTO inline_keyboards(bot_id,inline_message_id,reply_markup) VALUES(?,?,?) ON CONFLICT(bot_id,inline_message_id) DO UPDATE SET reply_markup=excluded.reply_markup`, botID, id, jsonText(markup))
	return err
}
func (s *Store) InlineKeyboard(botID int64, id string) (json.RawMessage, error) {
	var markup json.RawMessage
	err := s.db.QueryRow("SELECT reply_markup FROM inline_keyboards WHERE bot_id=? AND inline_message_id=?", botID, id).Scan(jsonScan(&markup))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return markup, err
}

func (s *Store) AdvanceTelegramOffset(botID, offset int64) error {
	_, err := s.db.Exec("UPDATE bots SET offset_id=MAX(offset_id,?) WHERE id=?", offset, botID)
	return err
}
