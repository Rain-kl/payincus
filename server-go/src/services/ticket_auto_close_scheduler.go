// Port of server/src/services/ticket-auto-close-scheduler.ts.
//
// Every hour (and once at startup) resolved tickets whose last public message came
// from the host owner and is older than the configured timeout are closed.
//
// Details that matter:
//
//   - the feature switch defaults to ON (`getSystemConfigBoolean(..., true)`);
//   - a configured timeout below 1 hour falls back to 24h rather than closing
//     everything;
//   - `autoCloseTickets` only returns the rows this round actually claimed, so a
//     concurrent admin reply is never announced as "auto-closed";
//   - notification/log failures are caught per ticket, not per round.
package services

import (
	"context"
	"log"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

const (
	// TicketAutoCloseEnabledConfigKey mirrors the system-config key.
	TicketAutoCloseEnabledConfigKey = "ticket_auto_close_enabled"
	// TicketAutoCloseHoursConfigKey mirrors the system-config key.
	TicketAutoCloseHoursConfigKey = "ticket_auto_close_hours"
	// DefaultTicketAutoCloseHours is DEFAULT_TICKET_AUTO_CLOSE_HOURS.
	DefaultTicketAutoCloseHours = 24
	// TicketAutoCloseCheckInterval is CHECK_INTERVAL_MS (1 hour).
	TicketAutoCloseCheckInterval = time.Hour
)

// Seams for the per-ticket side effects. Production uses the notifier/log writer;
// tests swap them to prove one ticket's failure does not abort the batch.
var (
	ticketAutoCloseSendNotification = func(ctx context.Context, userID int32, subject, hostName string) {
		lib.SendNotification(ctx, int64(userID), lib.EventTicketAutoClosed, lib.EventData{
			"subject":  subject,
			"hostName": hostName,
		})
	}
	ticketAutoCloseWriteLog = func(ctx context.Context, userID int32, ticketID int32, subject string, hours int) {
		db.CreateLog(ctx, int64Pointer(int64(userID)), "ticket", "ticket.auto_closed",
			"Ticket #"+itoa(ticketID)+" \""+subject+"\" auto-closed after "+itoa(int32(hours))+" hours",
			"success", db.CreateLogOptions{})
	}
	ticketAutoCloseLoadHostName = func(ctx context.Context, hostID *int32) (string, error) {
		if hostID == nil {
			return "系统", nil
		}
		host, err := db.HostsGetByID(ctx, *hostID)
		if err != nil {
			return "", err
		}
		if host == nil || host.Name == "" {
			return "系统", nil
		}
		return host.Name, nil
	}
)

// TicketAutoCloseSchedulerNextRun mirrors `setInterval(…, 1h)` (the immediate
// startup run is separate).
func TicketAutoCloseSchedulerNextRun(now time.Time) time.Time {
	return now.UTC().Add(TicketAutoCloseCheckInterval)
}

// TicketAutoCloseTimeout isolates the "configured hours → timeout" rule. A value
// below one hour is ignored on purpose: a zero/mis-typed config must not close
// every resolved ticket on the next tick.
func TicketAutoCloseTimeout(configuredHours int) time.Duration {
	hours := configuredHours
	if hours < 1 {
		hours = DefaultTicketAutoCloseHours
	}
	return time.Duration(hours) * time.Hour
}

// ResolveTicketAutoCloseHours returns the effective hour count for logging.
func ResolveTicketAutoCloseHours(configuredHours int) int {
	if configuredHours < 1 {
		return DefaultTicketAutoCloseHours
	}
	return configuredHours
}

// ProcessTicketAutoClose mirrors processTicketAutoClose.
func ProcessTicketAutoClose(ctx context.Context) {
	enabled, err := db.GetSystemConfigBoolean(ctx, TicketAutoCloseEnabledConfigKey, true)
	if err != nil {
		log.Printf("[TicketAutoClose] 处理失败: %v", err)
		return
	}
	if !enabled {
		return
	}

	configuredHours, err := db.GetSystemConfigNumber(ctx, TicketAutoCloseHoursConfigKey, DefaultTicketAutoCloseHours)
	if err != nil {
		log.Printf("[TicketAutoClose] 处理失败: %v", err)
		return
	}
	autoCloseHours := ResolveTicketAutoCloseHours(configuredHours)
	timeout := TicketAutoCloseTimeout(configuredHours)
	timeoutMs := timeout.Milliseconds()

	tickets, err := db.TicketsGetTicketsForAutoClose(ctx, timeoutMs)
	if err != nil {
		log.Printf("[TicketAutoClose] 处理失败: %v", err)
		return
	}
	if len(tickets) == 0 {
		return
	}
	log.Printf("[TicketAutoClose] 发现 %d 个需要自动关闭的工单", len(tickets))

	ids := make([]int32, 0, len(tickets))
	for _, ticket := range tickets {
		ids = append(ids, ticket.ID)
	}
	closed, err := db.TicketsAutoCloseTickets(ctx, ids, timeoutMs)
	if err != nil {
		log.Printf("[TicketAutoClose] 处理失败: %v", err)
		return
	}

	processAutoClosedTickets(ctx, closed, autoCloseHours)
	log.Printf("[TicketAutoClose] 成功自动关闭 %d 个工单", len(closed))
}

// processAutoClosedTickets mirrors the notification/log loop of
// processTicketAutoClose. Every ticket is isolated: a failing notification or log
// write is logged and the loop continues.
func processAutoClosedTickets(ctx context.Context, closed []db.TicketsAutoClosedTicket, autoCloseHours int) {
	for _, ticket := range closed {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Printf("[TicketAutoClose] 通知/日志失败 (工单 #%d): %v", ticket.ID, recovered)
				}
			}()

			hostName, hostErr := ticketAutoCloseLoadHostName(ctx, ticket.HostID)
			if hostErr != nil {
				log.Printf("[TicketAutoClose] 通知/日志失败 (工单 #%d): %v", ticket.ID, hostErr)
				return
			}

			ticketAutoCloseSendNotification(ctx, ticket.UserID, ticket.Subject, hostName)
			ticketAutoCloseWriteLog(ctx, ticket.UserID, ticket.ID, ticket.Subject, autoCloseHours)
		}()
	}
}

// startTicketAutoCloseScheduler wires the hourly interval with an immediate run.
func startTicketAutoCloseScheduler(group *schedulerGroup) {
	group.startIntervalLoop("TicketAutoClose", TicketAutoCloseCheckInterval, true, func(ctx context.Context) error {
		ProcessTicketAutoClose(ctx)
		return nil
	}, nil)
}
