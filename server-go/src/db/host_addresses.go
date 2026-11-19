// Port of server/src/db/host-addresses.ts.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// HostAddressAliasInput mirrors HostAddressAliasInput in host-addresses.ts.
type HostAddressAliasInput struct {
	Address string
	Kind    models.HostAddressKind
	Source  models.HostAddressSource
}

// HostAddressConflictInfo mirrors HostAddressConflictInfo in host-addresses.ts.
type HostAddressConflictInfo struct {
	ID         int32  `json:"id"`
	Address    string `json:"address"`
	HostAID    int32  `json:"hostAId"`
	HostAName  string `json:"hostAName"`
	HostBID    int32  `json:"hostBId"`
	HostBName  string `json:"hostBName"`
	WasCreated bool   `json:"wasCreated"`
}

// HostAddressConflictAlias is a host address alias joined with its host name/url.
type HostAddressConflictAlias struct {
	HostID   int32  `gorm:"column:host_id"`
	HostName string `gorm:"column:host_name"`
	HostURL  string `gorm:"column:host_url"`
	Address  string `gorm:"column:address"`
}

// HostAddressSummaryHost mirrors the host projection for backfill and polling.
type HostAddressSummaryHost struct {
	ID   int32  `gorm:"column:id"`
	Name string `gorm:"column:name"`
	URL  string `gorm:"column:url"`
}

// HostAddressResolutionLogInput mirrors createHostAddressResolutionLog input.
type HostAddressResolutionLogInput struct {
	HostID            int32
	InputAddress      string
	InputKind         models.HostAddressKind
	Trigger           models.HostAddressCheckTrigger
	Status            models.HostAddressCheckStatus
	ResolvedAddresses []string
	Error             *string
	Details           map[string]any
}

func hostAddressDB(client *gorm.DB) (*gorm.DB, error) {
	if client != nil {
		return client, nil
	}
	if DB == nil {
		return nil, errors.New("database not initialized")
	}
	return DB, nil
}

// UpsertHostInputAlias mirrors upsertHostInputAlias.
func UpsertHostInputAlias(ctx context.Context, hostID int32, alias HostAddressAliasInput, client *gorm.DB) error {
	dbConn, err := hostAddressDB(client)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return dbConn.WithContext(ctx).Exec(`
		INSERT INTO host_address_aliases (host_id, address, kind, source, created_at, updated_at)
		VALUES (?, ?, ?::"HostAddressKind", ?::"HostAddressSource", ?, ?)
		ON CONFLICT (host_id, address) DO UPDATE
		SET kind = EXCLUDED.kind, source = EXCLUDED.source, updated_at = EXCLUDED.updated_at`,
		hostID, alias.Address, string(alias.Kind), string(alias.Source), now, now,
	).Error
}

// ReplaceHostResolvedAliases mirrors replaceHostResolvedAliases.
func ReplaceHostResolvedAliases(ctx context.Context, hostID int32, aliases []HostAddressAliasInput, client *gorm.DB) error {
	dbConn, err := hostAddressDB(client)
	if err != nil {
		return err
	}
	if err := dbConn.WithContext(ctx).Exec(
		`DELETE FROM host_address_aliases WHERE host_id = ? AND source = 'resolved'`, hostID,
	).Error; err != nil {
		return err
	}
	if len(aliases) == 0 {
		return nil
	}
	now := time.Now().UTC()
	for _, a := range aliases {
		if err := dbConn.WithContext(ctx).Exec(`
			INSERT INTO host_address_aliases (host_id, address, kind, source, created_at, updated_at)
			VALUES (?, ?, ?::"HostAddressKind", ?::"HostAddressSource", ?, ?)
			ON CONFLICT (host_id, address) DO NOTHING`,
			hostID, a.Address, string(a.Kind), string(a.Source), now, now,
		).Error; err != nil {
			return err
		}
	}
	return nil
}

// DeleteStaleHostInputAliases removes old source='input' aliases other than keepAddress.
func DeleteStaleHostInputAliases(ctx context.Context, hostID int32, keepAddress string, client *gorm.DB) error {
	dbConn, err := hostAddressDB(client)
	if err != nil {
		return err
	}
	return dbConn.WithContext(ctx).Exec(
		`DELETE FROM host_address_aliases WHERE host_id = ? AND source = 'input' AND address <> ?`,
		hostID, keepAddress,
	).Error
}

// GetHostAddressAliasesForHost lists all currently recorded alias addresses for hostID.
func GetHostAddressAliasesForHost(ctx context.Context, hostID int32, client *gorm.DB) ([]string, error) {
	dbConn, err := hostAddressDB(client)
	if err != nil {
		return nil, err
	}
	var addrs []string
	err = dbConn.WithContext(ctx).Raw(
		`SELECT address FROM host_address_aliases WHERE host_id = ?`, hostID,
	).Scan(&addrs).Error
	return addrs, err
}

// GetAliasesByAddresses mirrors getAliasesByAddresses.
func GetAliasesByAddresses(ctx context.Context, addresses []string, excludeHostID *int32, client *gorm.DB) ([]HostAddressConflictAlias, error) {
	if len(addresses) == 0 {
		return []HostAddressConflictAlias{}, nil
	}
	dbConn, err := hostAddressDB(client)
	if err != nil {
		return nil, err
	}
	var rows []HostAddressConflictAlias
	query := `SELECT a.host_id, h.name AS host_name, h.url AS host_url, a.address
		FROM host_address_aliases a
		JOIN hosts h ON h.id = a.host_id
		WHERE a.address IN ?`
	args := []any{addresses}
	if excludeHostID != nil {
		query += ` AND a.host_id <> ?`
		args = append(args, *excludeHostID)
	}
	err = dbConn.WithContext(ctx).Raw(query, args...).Scan(&rows).Error
	return rows, err
}

// CreateHostAddressResolutionLog mirrors createHostAddressResolutionLog.
func CreateHostAddressResolutionLog(ctx context.Context, data HostAddressResolutionLogInput, client *gorm.DB) error {
	dbConn, err := hostAddressDB(client)
	if err != nil {
		return err
	}
	resJSON, _ := json.Marshal(data.ResolvedAddresses)
	if data.ResolvedAddresses == nil {
		resJSON = []byte("[]")
	}
	details := data.Details
	if details == nil {
		details = map[string]any{}
	}
	detailsJSON, _ := json.Marshal(details)
	return dbConn.WithContext(ctx).Exec(`
		INSERT INTO host_address_resolution_logs
			(host_id, input_address, input_kind, trigger, status, resolved_addresses, error, details, created_at)
		VALUES (?, ?, ?::"HostAddressKind", ?::"HostAddressCheckTrigger", ?::"HostAddressCheckStatus", ?::jsonb, ?, ?::jsonb, ?)`,
		data.HostID, data.InputAddress, string(data.InputKind), string(data.Trigger), string(data.Status),
		string(resJSON), data.Error, string(detailsJSON), time.Now().UTC(),
	).Error
}

// GetHostsForAddressBackfill mirrors getHostsForAddressBackfill.
func GetHostsForAddressBackfill(ctx context.Context) ([]HostAddressSummaryHost, error) {
	if DB == nil {
		return nil, errors.New("database not initialized")
	}
	var hosts []HostAddressSummaryHost
	err := DB.WithContext(ctx).Raw(
		`SELECT id, name, url FROM hosts WHERE tunnel_enabled = false ORDER BY id ASC`,
	).Scan(&hosts).Error
	return hosts, err
}

// GetHostsWithDomainInputAlias mirrors getHostsWithDomainInputAlias.
func GetHostsWithDomainInputAlias(ctx context.Context) ([]HostAddressSummaryHost, error) {
	if DB == nil {
		return nil, errors.New("database not initialized")
	}
	var hosts []HostAddressSummaryHost
	err := DB.WithContext(ctx).Raw(`
		SELECT h.id, h.name, h.url
		FROM hosts h
		WHERE h.tunnel_enabled = false
		  AND EXISTS (
			SELECT 1 FROM host_address_aliases a
			WHERE a.host_id = h.id AND a.source = 'input' AND a.kind = 'domain'
		  )
		ORDER BY h.id ASC`,
	).Scan(&hosts).Error
	return hosts, err
}

// RemoveHostAddressAliases mirrors removeHostAddressAliases.
func RemoveHostAddressAliases(ctx context.Context, hostID int32, client *gorm.DB) ([]string, error) {
	dbConn, err := hostAddressDB(client)
	if err != nil {
		return nil, err
	}
	existing, err := GetHostAddressAliasesForHost(ctx, hostID, dbConn)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return nil, nil
	}
	if err := dbConn.WithContext(ctx).Exec(`DELETE FROM host_address_aliases WHERE host_id = ?`, hostID).Error; err != nil {
		return nil, err
	}
	return existing, nil
}

// CleanupTunnelHostAddressAliases mirrors cleanupTunnelHostAddressAliases.
func CleanupTunnelHostAddressAliases(ctx context.Context, client *gorm.DB) ([]HostAddressConflictInfo, error) {
	dbConn, err := hostAddressDB(client)
	if err != nil {
		return nil, err
	}
	var tunnelHostIDs []int32
	if err := dbConn.WithContext(ctx).Raw(`SELECT id FROM hosts WHERE tunnel_enabled = true`).Scan(&tunnelHostIDs).Error; err != nil {
		return nil, err
	}
	if len(tunnelHostIDs) == 0 {
		return nil, nil
	}
	var addresses []string
	if err := dbConn.WithContext(ctx).Raw(
		`SELECT DISTINCT address FROM host_address_aliases WHERE host_id IN ?`, tunnelHostIDs,
	).Scan(&addresses).Error; err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, nil
	}
	if err := dbConn.WithContext(ctx).Exec(
		`DELETE FROM host_address_aliases WHERE host_id IN ?`, tunnelHostIDs,
	).Error; err != nil {
		return nil, err
	}
	return SyncHostAddressConflicts(ctx, addresses, dbConn)
}

// SyncHostAddressConflicts mirrors syncHostAddressConflicts.
func SyncHostAddressConflicts(ctx context.Context, addresses []string, client *gorm.DB) ([]HostAddressConflictInfo, error) {
	if len(addresses) == 0 {
		return nil, nil
	}
	dbConn, err := hostAddressDB(client)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	uniqueAddrs := make([]string, 0, len(addresses))
	for _, a := range addresses {
		if a != "" && !seen[a] {
			seen[a] = true
			uniqueAddrs = append(uniqueAddrs, a)
		}
	}
	if len(uniqueAddrs) == 0 {
		return nil, nil
	}

	var rows []HostAddressConflictAlias
	if err := dbConn.WithContext(ctx).Raw(`
		SELECT a.host_id, h.name AS host_name, a.address
		FROM host_address_aliases a
		JOIN hosts h ON h.id = a.host_id
		WHERE a.address IN ?
		ORDER BY a.address ASC, a.host_id ASC`, uniqueAddrs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	byAddr := map[string][]HostAddressConflictAlias{}
	for _, r := range rows {
		byAddr[r.Address] = append(byAddr[r.Address], r)
	}

	desiredKeys := map[string]bool{}
	var created []HostAddressConflictInfo
	now := time.Now().UTC()

	for _, addr := range uniqueAddrs {
		group := byAddr[addr]
		hostMap := map[int32]string{}
		for _, item := range group {
			hostMap[item.HostID] = item.HostName
		}
		if len(hostMap) < 2 {
			continue
		}
		hostIDs := make([]int32, 0, len(hostMap))
		for id := range hostMap {
			hostIDs = append(hostIDs, id)
		}
		sort.Slice(hostIDs, func(i, j int) bool { return hostIDs[i] < hostIDs[j] })

		for i := 0; i < len(hostIDs); i++ {
			for j := i + 1; j < len(hostIDs); j++ {
				hostAID, hostBID := hostIDs[i], hostIDs[j]
				hostAName, hostBName := hostMap[hostAID], hostMap[hostBID]
				desiredKeys[hostConflictKey(hostAID, hostBID, addr)] = true

				var existing struct {
					ID     int32  `gorm:"column:id"`
					Status string `gorm:"column:status"`
				}
				res := dbConn.WithContext(ctx).Raw(`
					SELECT id, status::text AS status
					FROM host_address_conflicts
					WHERE host_a_id = ? AND host_b_id = ? AND address = ?
					LIMIT 1`, hostAID, hostBID, addr,
				).Scan(&existing)
				if res.Error != nil {
					return nil, res.Error
				}
				if res.RowsAffected == 0 {
					var newID int32
					if err := dbConn.WithContext(ctx).Raw(`
						INSERT INTO host_address_conflicts (host_a_id, host_b_id, address, status, first_detected_at, last_detected_at)
						VALUES (?, ?, ?, 'active'::"HostAddressConflictStatus", ?, ?)
						RETURNING id`, hostAID, hostBID, addr, now, now,
					).Scan(&newID).Error; err != nil {
						return nil, err
					}
					created = append(created, HostAddressConflictInfo{
						ID: newID, Address: addr,
						HostAID: hostAID, HostAName: hostAName,
						HostBID: hostBID, HostBName: hostBName,
						WasCreated: true,
					})
					continue
				}
				if existing.Status == "resolved" {
					if err := dbConn.WithContext(ctx).Exec(`
						UPDATE host_address_conflicts
						SET status = 'active'::"HostAddressConflictStatus", resolved_at = NULL, last_detected_at = ?
						WHERE id = ?`, now, existing.ID,
					).Error; err != nil {
						return nil, err
					}
					created = append(created, HostAddressConflictInfo{
						ID: existing.ID, Address: addr,
						HostAID: hostAID, HostAName: hostAName,
						HostBID: hostBID, HostBName: hostBName,
						WasCreated: true,
					})
					continue
				}
				if err := dbConn.WithContext(ctx).Exec(`
					UPDATE host_address_conflicts SET resolved_at = NULL, last_detected_at = ? WHERE id = ?`,
					now, existing.ID,
				).Error; err != nil {
					return nil, err
				}
			}
		}
	}

	type activeRow struct {
		ID      int32  `gorm:"column:id"`
		HostAID int32  `gorm:"column:host_a_id"`
		HostBID int32  `gorm:"column:host_b_id"`
		Address string `gorm:"column:address"`
	}
	var actives []activeRow
	if err := dbConn.WithContext(ctx).Raw(`
		SELECT id, host_a_id, host_b_id, address
		FROM host_address_conflicts
		WHERE address IN ? AND status = 'active'`, uniqueAddrs,
	).Scan(&actives).Error; err != nil {
		return nil, err
	}
	for _, c := range actives {
		if desiredKeys[hostConflictKey(c.HostAID, c.HostBID, c.Address)] {
			continue
		}
		if err := dbConn.WithContext(ctx).Exec(`
			UPDATE host_address_conflicts
			SET status = 'resolved'::"HostAddressConflictStatus", resolved_at = ?, last_detected_at = ?
			WHERE id = ?`, now, now, c.ID,
		).Error; err != nil {
			return nil, err
		}
	}

	return created, nil
}

func hostConflictKey(a, b int32, addr string) string {
	return string(rune(0)) + addr + ":" + int32Str(a) + ":" + int32Str(b)
}

func int32Str(v int32) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
