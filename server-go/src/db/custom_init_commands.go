// Port of server/src/db/custom-init-commands.ts.
//
// Every package-level identifier is prefixed with `customInit` because the db
// package is shared by many parallel ports.
//
// Behavioural notes carried over from the TypeScript:
//
//   - `distros` is stored as a JSON array string; every read parses it back.
//   - create validates the command size (64 KiB UTF-8), the per-user template
//     count (50) and the distro list, in that order, before inserting.
//   - update re-checks ownership first (`NOT_FOUND` when the row is not owned),
//     then size/distros, then writes. The TypeScript always calls
//     `prisma.customInitCommand.update`, so an update body with no fields still
//     bumps `updated_at` (@updatedAt). There is deliberately no empty-update
//     early return here: that would diverge from the Node behaviour.
//   - create/update trim `name` and store `description` as NULL when the trimmed
//     value is empty.
package db

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"
)

const (
	// customInitMaxCommandsPerUser mirrors MAX_COMMANDS_PER_USER.
	customInitMaxCommandsPerUser = 50
	// customInitMaxCommandSize mirrors MAX_COMMAND_SIZE (64 KiB, UTF-8 bytes).
	customInitMaxCommandSize = 64 * 1024
)

// customInitValidDistros mirrors the validDistros list repeated in the db layer.
var customInitValidDistros = []string{"ubuntu", "debian", "rhel", "alpine", "arch", "suse", "all"}

// CustomInitCommandData mirrors the CustomInitCommandData interface. The
// timestamps are ISO strings because the TypeScript calls Date#toISOString().
type CustomInitCommandData struct {
	ID          int32    `json:"id"`
	UserID      int32    `json:"userId"`
	Name        string   `json:"name"`
	Command     string   `json:"command"`
	Distros     []string `json:"distros"`
	Description *string  `json:"description"`
	Enabled     bool     `json:"enabled"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

// CustomInitCommandCreateData mirrors createCustomInitCommand's argument.
type CustomInitCommandCreateData struct {
	UserID      int32
	Name        string
	Command     string
	Distros     []string
	Description *string
}

// CustomInitCommandUpdateData mirrors updateCustomInitCommand's argument. Go
// cannot express `data.distros !== undefined` / `data.description !== undefined`
// with plain pointers (an explicit JSON null is a present key), so the
// *Provided flags carry the "was the key present" bit.
type CustomInitCommandUpdateData struct {
	Name           *string
	Command        *string
	Distros        []string
	DistrosSet     bool
	Description    *string
	DescriptionSet bool
	// Enabled is a pointer on purpose: `enabled !== undefined` is tri-state
	// (absent vs explicit false).
	Enabled *bool
}

// CustomInitCommandCreateResult mirrors `{ success: true; id } | { success:
// false; error }`.
type CustomInitCommandCreateResult struct {
	Success bool
	ID      int32
	Error   string
}

// CustomInitCommandUpdateResult mirrors `{ success: true } | { success: false;
// error }`.
type CustomInitCommandUpdateResult struct {
	Success bool
	Error   string
}

// CustomInitCommandOwnership mirrors validateCommandsOwnership's union.
type CustomInitCommandOwnership struct {
	Valid    bool
	Commands []CustomInitCommandData
	Error    string
}

// customInitCommandRow is the raw SQL projection (column names, not Prisma names).
type customInitCommandRow struct {
	ID          int32     `gorm:"column:id"`
	UserID      int32     `gorm:"column:user_id"`
	Name        string    `gorm:"column:name"`
	Command     string    `gorm:"column:command"`
	Distros     string    `gorm:"column:distros"`
	Description *string   `gorm:"column:description"`
	Enabled     bool      `gorm:"column:enabled"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

const customInitCommandSelectColumns = `
	id, user_id, name, command, distros, description, enabled, created_at, updated_at`

// customInitParseDistros mirrors `JSON.parse(cmd.distros) as string[]`.
func customInitParseDistros(raw string) ([]string, error) {
	var parsed []string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// customInitJSONString mirrors JSON.stringify for the stored distro list.
func customInitJSONString(values []string) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(values); err != nil {
		return "[]"
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

// customInitInvalidDistros mirrors `distros.filter(d => !validDistros.includes(d))`.
func customInitInvalidDistros(distros []string) []string {
	invalid := make([]string, 0)
	for _, distro := range distros {
		valid := false
		for _, candidate := range customInitValidDistros {
			if candidate == distro {
				valid = true
				break
			}
		}
		if !valid {
			invalid = append(invalid, distro)
		}
	}
	return invalid
}

// customInitNormalizeDescription mirrors `value?.trim() || null`.
func customInitNormalizeDescription(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func customInitMapRow(row customInitCommandRow) (CustomInitCommandData, error) {
	distros, err := customInitParseDistros(row.Distros)
	if err != nil {
		return CustomInitCommandData{}, err
	}
	return CustomInitCommandData{
		ID:          row.ID,
		UserID:      row.UserID,
		Name:        row.Name,
		Command:     row.Command,
		Distros:     distros,
		Description: row.Description,
		Enabled:     row.Enabled,
		CreatedAt:   isoTimestamp(row.CreatedAt),
		UpdatedAt:   isoTimestamp(row.UpdatedAt),
	}, nil
}

func customInitQueryRows(ctx context.Context, sql string, args ...any) ([]customInitCommandRow, error) {
	var rows []customInitCommandRow
	if err := DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func customInitMapRows(rows []customInitCommandRow) ([]CustomInitCommandData, error) {
	commands := make([]CustomInitCommandData, 0, len(rows))
	for _, row := range rows {
		mapped, err := customInitMapRow(row)
		if err != nil {
			return nil, err
		}
		commands = append(commands, mapped)
	}
	return commands, nil
}

// GetCustomInitCommandsByUserId mirrors getCustomInitCommandsByUserId: newest
// first.
func GetCustomInitCommandsByUserId(ctx context.Context, userID int32) ([]CustomInitCommandData, error) {
	rows, err := customInitQueryRows(ctx,
		"SELECT "+customInitCommandSelectColumns+
			" FROM custom_init_commands WHERE user_id = ? ORDER BY created_at DESC", userID)
	if err != nil {
		return nil, err
	}
	return customInitMapRows(rows)
}

// GetEnabledCommandsByDistro mirrors getEnabledCommandsByDistro: enabled rows
// whose distro list contains the requested distro or "all", newest first.
func GetEnabledCommandsByDistro(ctx context.Context, userID int32, distro string) ([]CustomInitCommandData, error) {
	rows, err := customInitQueryRows(ctx,
		"SELECT "+customInitCommandSelectColumns+
			" FROM custom_init_commands WHERE user_id = ? AND enabled = true ORDER BY created_at DESC",
		userID)
	if err != nil {
		return nil, err
	}

	filtered := make([]CustomInitCommandData, 0, len(rows))
	for _, row := range rows {
		mapped, err := customInitMapRow(row)
		if err != nil {
			return nil, err
		}
		matches := false
		for _, candidate := range mapped.Distros {
			if candidate == "all" || candidate == distro {
				matches = true
				break
			}
		}
		if matches {
			filtered = append(filtered, mapped)
		}
	}
	return filtered, nil
}

// GetCustomInitCommandById mirrors getCustomInitCommandById (any owner).
func GetCustomInitCommandById(ctx context.Context, id int32) (*CustomInitCommandData, error) {
	rows, err := customInitQueryRows(ctx,
		"SELECT "+customInitCommandSelectColumns+" FROM custom_init_commands WHERE id = ? LIMIT 1", id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	mapped, err := customInitMapRow(rows[0])
	if err != nil {
		return nil, err
	}
	return &mapped, nil
}

// GetCustomInitCommandsByIds mirrors getCustomInitCommandsByIds. An empty id
// list short-circuits to an empty slice without touching the database.
func GetCustomInitCommandsByIds(ctx context.Context, ids []int32) ([]CustomInitCommandData, error) {
	if len(ids) == 0 {
		return []CustomInitCommandData{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := customInitQueryRows(ctx,
		"SELECT "+customInitCommandSelectColumns+
			" FROM custom_init_commands WHERE id IN ("+placeholders+")", args...)
	if err != nil {
		return nil, err
	}
	return customInitMapRows(rows)
}

// GetCustomInitCommandCount mirrors getCustomInitCommandCount.
func GetCustomInitCommandCount(ctx context.Context, userID int32) (int64, error) {
	var count int64
	if err := DB.WithContext(ctx).
		Raw("SELECT COUNT(*) FROM custom_init_commands WHERE user_id = ?", userID).
		Scan(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// CreateCustomInitCommand mirrors createCustomInitCommand: size, then count,
// then distro validation, then insert.
func CreateCustomInitCommand(ctx context.Context, data CustomInitCommandCreateData) (CustomInitCommandCreateResult, error) {
	if len(data.Command) > customInitMaxCommandSize {
		return CustomInitCommandCreateResult{Success: false, Error: "COMMAND_TOO_LARGE"}, nil
	}

	count, err := GetCustomInitCommandCount(ctx, data.UserID)
	if err != nil {
		return CustomInitCommandCreateResult{}, err
	}
	if count >= customInitMaxCommandsPerUser {
		return CustomInitCommandCreateResult{Success: false, Error: "MAX_COMMANDS_REACHED"}, nil
	}

	if len(customInitInvalidDistros(data.Distros)) > 0 {
		return CustomInitCommandCreateResult{Success: false, Error: "INVALID_DISTROS"}, nil
	}

	now := time.Now().UTC()
	var id int32
	if err := DB.WithContext(ctx).Raw(
		"INSERT INTO custom_init_commands (user_id, name, command, distros, description, enabled, created_at, updated_at)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		data.UserID,
		strings.TrimSpace(data.Name),
		data.Command,
		customInitJSONString(data.Distros),
		customInitNormalizeDescription(data.Description),
		true,
		now,
		now,
	).Scan(&id).Error; err != nil {
		return CustomInitCommandCreateResult{}, err
	}

	return CustomInitCommandCreateResult{Success: true, ID: id}, nil
}

// UpdateCustomInitCommand mirrors updateCustomInitCommand.
func UpdateCustomInitCommand(ctx context.Context, id int32, userID int32, data CustomInitCommandUpdateData) (CustomInitCommandUpdateResult, error) {
	rows, err := customInitQueryRows(ctx,
		"SELECT id FROM custom_init_commands WHERE id = ? AND user_id = ? LIMIT 1", id, userID)
	if err != nil {
		return CustomInitCommandUpdateResult{}, err
	}
	if len(rows) == 0 {
		return CustomInitCommandUpdateResult{Success: false, Error: "NOT_FOUND"}, nil
	}

	if data.Command != nil && len(*data.Command) > customInitMaxCommandSize {
		return CustomInitCommandUpdateResult{Success: false, Error: "COMMAND_TOO_LARGE"}, nil
	}

	if data.DistrosSet && len(customInitInvalidDistros(data.Distros)) > 0 {
		return CustomInitCommandUpdateResult{Success: false, Error: "INVALID_DISTROS"}, nil
	}

	assignments := make([]string, 0, 6)
	args := make([]any, 0, 7)

	if data.Name != nil {
		assignments = append(assignments, "name = ?")
		args = append(args, strings.TrimSpace(*data.Name))
	}
	if data.Command != nil {
		assignments = append(assignments, "command = ?")
		args = append(args, *data.Command)
	}
	if data.DistrosSet {
		assignments = append(assignments, "distros = ?")
		args = append(args, customInitJSONString(data.Distros))
	}
	if data.DescriptionSet {
		assignments = append(assignments, "description = ?")
		args = append(args, customInitNormalizeDescription(data.Description))
	}
	if data.Enabled != nil {
		assignments = append(assignments, "enabled = ?")
		args = append(args, *data.Enabled)
	}

	// Prisma's @updatedAt bumps on every update() call, including an update with
	// no field changes, so this is unconditional (see the file header).
	assignments = append(assignments, "updated_at = ?")
	args = append(args, time.Now().UTC())
	args = append(args, id)

	if err := DB.WithContext(ctx).Exec(
		"UPDATE custom_init_commands SET "+strings.Join(assignments, ", ")+" WHERE id = ?",
		args...,
	).Error; err != nil {
		return CustomInitCommandUpdateResult{}, err
	}

	return CustomInitCommandUpdateResult{Success: true}, nil
}

// DeleteCustomInitCommand mirrors deleteCustomInitCommand: the delete is scoped
// to the owner, and a zero row count means "not found".
func DeleteCustomInitCommand(ctx context.Context, id int32, userID int32) (bool, error) {
	result := DB.WithContext(ctx).
		Exec("DELETE FROM custom_init_commands WHERE id = ? AND user_id = ?", id, userID)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// ValidateCustomInitCommandsOwnership mirrors validateCommandsOwnership: every
// requested id must exist, belong to the user and be enabled.
func ValidateCustomInitCommandsOwnership(ctx context.Context, commandIDs []int32, userID int32) (CustomInitCommandOwnership, error) {
	if len(commandIDs) == 0 {
		return CustomInitCommandOwnership{Valid: true, Commands: []CustomInitCommandData{}}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(commandIDs)), ",")
	args := make([]any, 0, len(commandIDs)+1)
	for _, id := range commandIDs {
		args = append(args, id)
	}
	args = append(args, userID)

	rows, err := customInitQueryRows(ctx,
		"SELECT "+customInitCommandSelectColumns+
			" FROM custom_init_commands WHERE id IN ("+placeholders+") AND user_id = ? AND enabled = true",
		args...)
	if err != nil {
		return CustomInitCommandOwnership{}, err
	}
	if len(rows) != len(commandIDs) {
		return CustomInitCommandOwnership{Valid: false, Error: "INVALID_COMMAND_IDS"}, nil
	}

	commands, err := customInitMapRows(rows)
	if err != nil {
		return CustomInitCommandOwnership{}, err
	}
	return CustomInitCommandOwnership{Valid: true, Commands: commands}, nil
}

// MergeCustomInitCommandContents mirrors mergeCommandContents: the templates are
// joined with a newline, in the order the caller supplied them.
func MergeCustomInitCommandContents(commands []CustomInitCommandData) string {
	if len(commands) == 0 {
		return ""
	}
	contents := make([]string, 0, len(commands))
	for _, command := range commands {
		contents = append(contents, command.Command)
	}
	return strings.Join(contents, "\n")
}

// GetImageDistroFromAlias mirrors getImageDistroFromAlias. The order of the
// checks is significant and must stay identical.
func GetImageDistroFromAlias(imageAlias string) string {
	alias := strings.ToLower(imageAlias)

	if strings.Contains(alias, "ubuntu") {
		return "ubuntu"
	}
	if strings.Contains(alias, "debian") || strings.Contains(alias, "kali") {
		return "debian"
	}
	if strings.Contains(alias, "alpine") {
		return "alpine"
	}
	if strings.Contains(alias, "arch") {
		return "arch"
	}
	if strings.Contains(alias, "opensuse") || strings.Contains(alias, "suse") ||
		strings.Contains(alias, "tumbleweed") || strings.Contains(alias, "leap") {
		return "suse"
	}
	if strings.Contains(alias, "alma") || strings.Contains(alias, "rocky") ||
		strings.Contains(alias, "oracle") || strings.Contains(alias, "centos") ||
		strings.Contains(alias, "rhel") || strings.Contains(alias, "fedora") {
		return "rhel"
	}

	return "other"
}
