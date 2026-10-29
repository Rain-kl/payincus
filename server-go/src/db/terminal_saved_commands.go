// Port of server/src/db/terminal-saved-commands.ts.
//
// The TypeScript module encrypts the stored columns through lib/security.ts
// (encryptSensitiveData / decryptSensitiveData / isEncrypted). The db package
// must not import lib, so the two encrypt/decrypt primitives are the ones
// already mirrored in db/system_config.go and only the isEncrypted() predicate
// is added here.
package db

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

const (
	// maxTerminalCommandsPerUser mirrors MAX_COMMANDS_PER_USER.
	maxTerminalCommandsPerUser = 100
	// maxTerminalCommandSize mirrors MAX_COMMAND_SIZE (16 KB, UTF-8 bytes).
	maxTerminalCommandSize = 16 * 1024
	// terminalSavedCommandLockNamespace mirrors
	// TERMINAL_SAVED_COMMAND_LOCK_NAMESPACE. It is a cross-process contract that
	// must stay identical to the TypeScript value.
	terminalSavedCommandLockNamespace = 4124
	// terminalSavedCommandEncryptionTagLength mirrors ENCRYPTION_CONFIG.tagLength
	// (the GCM tag is 16 bytes, like the IV).
	terminalSavedCommandEncryptionTagLength = 16
)

var (
	// /[\r\n\t]+/g
	terminalSavedCommandLineBreaks = regexp.MustCompile(`[\r\n\t]+`)
	// CONTROL_CHARS_REGEX = /[\u0000-\u001F\u007F]/g
	terminalSavedCommandControlChars = regexp.MustCompile(`[\x00-\x1F\x7F]`)
)

// TerminalSavedCommandData mirrors the TerminalSavedCommandData interface: the
// timestamps are ISO strings because the TypeScript calls Date#toISOString().
type TerminalSavedCommandData struct {
	ID          int32   `json:"id"`
	UserID      int32   `json:"userId"`
	Name        string  `json:"name"`
	Command     string  `json:"command"`
	Description *string `json:"description"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

// TerminalSavedCommandCreateData mirrors the createTerminalSavedCommand() argument.
type TerminalSavedCommandCreateData struct {
	UserID      int32
	Name        string
	Command     string
	Description *string
}

// TerminalSavedCommandUpdateData mirrors the updateTerminalSavedCommand() argument.
// The *Provided flags carry the TypeScript `data.x !== undefined` test, which a
// nil pointer cannot express for an explicitly-null description.
type TerminalSavedCommandUpdateData struct {
	NameProvided        bool
	Name                string
	CommandProvided     bool
	Command             string
	DescriptionProvided bool
	Description         *string
}

// TerminalSavedCommandResult mirrors the `{ success: true, command } |
// { success: false, error }` union. DB failures surface through the error return,
// validation failures through Error.
type TerminalSavedCommandResult struct {
	Success bool
	Command TerminalSavedCommandData
	Error   string
}

// terminalSavedCommandRow is the raw SQL projection (column names, not Prisma
// field names).
type terminalSavedCommandRow struct {
	ID          int32     `gorm:"column:id"`
	UserID      int32     `gorm:"column:user_id"`
	Name        string    `gorm:"column:name"`
	Command     string    `gorm:"column:command"`
	Description *string   `gorm:"column:description"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// normalizeTerminalCommandLabel mirrors normalizeCommandLabel().
func normalizeTerminalCommandLabel(value string) string {
	spaced := terminalSavedCommandLineBreaks.ReplaceAllString(value, " ")
	stripped := terminalSavedCommandControlChars.ReplaceAllString(spaced, "")
	return strings.TrimSpace(stripped)
}

// normalizeTerminalDescription mirrors normalizeDescription().
func normalizeTerminalDescription(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}

	normalized := strings.ReplaceAll(*value, "\r\n", "\n")
	normalized = terminalSavedCommandControlChars.ReplaceAllString(normalized, "")
	normalized = strings.TrimSpace(normalized)
	if normalized == "" {
		return nil
	}
	return &normalized
}

// isEncryptedSensitiveData mirrors isEncrypted() in lib/security.ts: a
// `iv:tag:ciphertext` triple whose iv and tag are 32 hex characters each.
func isEncryptedSensitiveData(data string) bool {
	parts := strings.Split(data, ":")
	if len(parts) != 3 {
		return false
	}
	return len(parts[0]) == encryptionIVLength*2 &&
		len(parts[1]) == terminalSavedCommandEncryptionTagLength*2
}

// mapTerminalSavedCommand mirrors mapTerminalSavedCommand(): decrypt the stored
// values when they are ciphertext and render the timestamps as ISO strings.
func mapTerminalSavedCommand(row terminalSavedCommandRow) TerminalSavedCommandData {
	name := row.Name
	if isEncryptedSensitiveData(name) {
		if decrypted := decryptSensitiveData(name); decrypted != "" {
			name = decrypted
		}
	}

	command := row.Command
	if isEncryptedSensitiveData(command) {
		if decrypted := decryptSensitiveData(command); decrypted != "" {
			command = decrypted
		}
	}

	var description *string
	if row.Description != nil {
		description = row.Description
		if isEncryptedSensitiveData(*row.Description) {
			if decrypted := decryptSensitiveData(*row.Description); decrypted != "" {
				description = &decrypted
			}
		}
	}

	return TerminalSavedCommandData{
		ID:          row.ID,
		UserID:      row.UserID,
		Name:        name,
		Command:     command,
		Description: description,
		CreatedAt:   isoTimestamp(row.CreatedAt),
		UpdatedAt:   isoTimestamp(row.UpdatedAt),
	}
}

const terminalSavedCommandSelectColumns = `
	id, user_id, name, command, description, created_at, updated_at`

func queryTerminalSavedCommandRows(ctx context.Context, sql string, args ...any) ([]terminalSavedCommandRow, error) {
	var rows []terminalSavedCommandRow
	if err := DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetTerminalSavedCommandsByUserId mirrors getTerminalSavedCommandsByUserId().
func GetTerminalSavedCommandsByUserId(ctx context.Context, userID int32) ([]TerminalSavedCommandData, error) {
	rows, err := queryTerminalSavedCommandRows(ctx,
		"SELECT "+terminalSavedCommandSelectColumns+
			" FROM terminal_saved_commands WHERE user_id = ?"+
			" ORDER BY updated_at DESC, id DESC", userID)
	if err != nil {
		return nil, err
	}

	commands := make([]TerminalSavedCommandData, 0, len(rows))
	for _, row := range rows {
		commands = append(commands, mapTerminalSavedCommand(row))
	}
	return commands, nil
}

// GetTerminalSavedCommandById mirrors getTerminalSavedCommandById(); it returns
// nil when the command does not belong to the user (or does not exist).
func GetTerminalSavedCommandById(ctx context.Context, id int32, userID int32) (*TerminalSavedCommandData, error) {
	rows, err := queryTerminalSavedCommandRows(ctx,
		"SELECT "+terminalSavedCommandSelectColumns+
			" FROM terminal_saved_commands WHERE id = ? AND user_id = ? LIMIT 1", id, userID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	mapped := mapTerminalSavedCommand(rows[0])
	return &mapped, nil
}

// CreateTerminalSavedCommand mirrors createTerminalSavedCommand(): validate, then
// take the per-user advisory lock, count and insert inside one transaction.
func CreateTerminalSavedCommand(ctx context.Context, data TerminalSavedCommandCreateData) (TerminalSavedCommandResult, error) {
	name := normalizeTerminalCommandLabel(data.Name)
	command := data.Command
	description := normalizeTerminalDescription(data.Description)

	if name == "" {
		return TerminalSavedCommandResult{Success: false, Error: "INVALID_NAME"}, nil
	}
	if strings.TrimSpace(command) == "" {
		return TerminalSavedCommandResult{Success: false, Error: "INVALID_COMMAND"}, nil
	}
	if len(command) > maxTerminalCommandSize {
		return TerminalSavedCommandResult{Success: false, Error: "COMMAND_TOO_LARGE"}, nil
	}

	if pool == nil {
		return TerminalSavedCommandResult{}, errors.New("database not initialized")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return TerminalSavedCommandResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := AdvisoryTransactionLock(ctx, tx, terminalSavedCommandLockNamespace, data.UserID); err != nil {
		return TerminalSavedCommandResult{}, err
	}

	var count int64
	if err := tx.QueryRow(ctx,
		"SELECT COUNT(*)::int FROM terminal_saved_commands WHERE user_id = $1", data.UserID).
		Scan(&count); err != nil {
		return TerminalSavedCommandResult{}, err
	}
	if count >= maxTerminalCommandsPerUser {
		return TerminalSavedCommandResult{Success: false, Error: "MAX_COMMANDS_REACHED"}, nil
	}

	encryptedName, err := encryptSensitiveData(name)
	if err != nil {
		return TerminalSavedCommandResult{}, err
	}
	encryptedCommand, err := encryptSensitiveData(command)
	if err != nil {
		return TerminalSavedCommandResult{}, err
	}
	var encryptedDescription any
	if description != nil {
		value, err := encryptSensitiveData(*description)
		if err != nil {
			return TerminalSavedCommandResult{}, err
		}
		encryptedDescription = value
	}

	var row terminalSavedCommandRow
	err = tx.QueryRow(ctx,
		"INSERT INTO terminal_saved_commands (user_id, name, command, description, updated_at)"+
			" VALUES ($1, $2, $3, $4, NOW())"+
			" RETURNING "+terminalSavedCommandSelectColumns,
		data.UserID, encryptedName, encryptedCommand, encryptedDescription).
		Scan(&row.ID, &row.UserID, &row.Name, &row.Command, &row.Description, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		return TerminalSavedCommandResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return TerminalSavedCommandResult{}, err
	}

	return TerminalSavedCommandResult{Success: true, Command: mapTerminalSavedCommand(row)}, nil
}

// UpdateTerminalSavedCommand mirrors updateTerminalSavedCommand(). Only the
// provided fields are re-validated, re-encrypted and written.
func UpdateTerminalSavedCommand(ctx context.Context, id int32, userID int32, data TerminalSavedCommandUpdateData) (TerminalSavedCommandResult, error) {
	rows, err := queryTerminalSavedCommandRows(ctx,
		"SELECT "+terminalSavedCommandSelectColumns+
			" FROM terminal_saved_commands WHERE id = ? AND user_id = ? LIMIT 1", id, userID)
	if err != nil {
		return TerminalSavedCommandResult{}, err
	}
	if len(rows) == 0 {
		return TerminalSavedCommandResult{Success: false, Error: "NOT_FOUND"}, nil
	}
	existing := mapTerminalSavedCommand(rows[0])

	nextName := existing.Name
	if data.NameProvided {
		nextName = normalizeTerminalCommandLabel(data.Name)
	}
	nextCommand := existing.Command
	if data.CommandProvided {
		nextCommand = data.Command
	}
	nextDescription := existing.Description
	if data.DescriptionProvided {
		nextDescription = normalizeTerminalDescription(data.Description)
	}

	if nextName == "" {
		return TerminalSavedCommandResult{Success: false, Error: "INVALID_NAME"}, nil
	}
	if strings.TrimSpace(nextCommand) == "" {
		return TerminalSavedCommandResult{Success: false, Error: "INVALID_COMMAND"}, nil
	}
	if len(nextCommand) > maxTerminalCommandSize {
		return TerminalSavedCommandResult{Success: false, Error: "COMMAND_TOO_LARGE"}, nil
	}

	encryptedName, err := encryptSensitiveData(nextName)
	if err != nil {
		return TerminalSavedCommandResult{}, err
	}
	encryptedCommand, err := encryptSensitiveData(nextCommand)
	if err != nil {
		return TerminalSavedCommandResult{}, err
	}
	var encryptedDescription any
	if nextDescription != nil {
		value, err := encryptSensitiveData(*nextDescription)
		if err != nil {
			return TerminalSavedCommandResult{}, err
		}
		encryptedDescription = value
	}

	var updated []terminalSavedCommandRow
	if err := DB.WithContext(ctx).Raw(
		"UPDATE terminal_saved_commands"+
			" SET name = ?, command = ?, description = ?, updated_at = NOW()"+
			" WHERE id = ? AND user_id = ?"+
			" RETURNING "+terminalSavedCommandSelectColumns,
		encryptedName, encryptedCommand, encryptedDescription, id, userID).
		Scan(&updated).Error; err != nil {
		return TerminalSavedCommandResult{}, err
	}
	if len(updated) == 0 {
		return TerminalSavedCommandResult{Success: false, Error: "NOT_FOUND"}, nil
	}

	return TerminalSavedCommandResult{Success: true, Command: mapTerminalSavedCommand(updated[0])}, nil
}

// DeleteTerminalSavedCommand mirrors deleteTerminalSavedCommand(): the delete is
// scoped to the owner, and a zero row count means "not found".
func DeleteTerminalSavedCommand(ctx context.Context, id int32, userID int32) (bool, error) {
	result := DB.WithContext(ctx).
		Exec("DELETE FROM terminal_saved_commands WHERE id = ? AND user_id = ?", id, userID)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}
