package service

import (
	"context"
	"log"
	"time"

	"google.golang.org/grpc"

	kafkaprod "github.com/exbanka/account-service/internal/kafka"
	"github.com/exbanka/account-service/internal/model"
	clientpb "github.com/exbanka/contract/clientpb"
	kafkamsg "github.com/exbanka/contract/kafka"
)

// emitTimeout bounds the email-lookup gRPC call so an unhealthy client-service
// can't block the (already-committed) account operation indefinitely. The
// account write has already succeeded; notifications are best-effort side effects.
const emitTimeout = 5 * time.Second

// eventPublisher is the narrow Kafka surface AccountService needs to satisfy the
// "publish from the service layer" rule. Implemented by *kafkaprod.Producer.
type eventPublisher interface {
	PublishAccountCreated(ctx context.Context, msg kafkamsg.AccountCreatedMessage) error
	PublishAccountStatusChanged(ctx context.Context, msg kafkaprod.AccountStatusChangedMsg) error
	PublishAccountNameUpdated(ctx context.Context, msg kafkamsg.AccountNameUpdatedMessage) error
	PublishAccountLimitsUpdated(ctx context.Context, msg kafkamsg.AccountLimitsUpdatedMessage) error
	PublishGeneralNotification(ctx context.Context, msg kafkamsg.GeneralNotificationMessage) error
	SendEmail(ctx context.Context, msg kafkamsg.SendEmailMessage) error
}

// clientLookup is the subset of the client-service gRPC client used to resolve a
// client's email for the account-created notification. Implemented by
// clientpb.ClientServiceClient.
type clientLookup interface {
	GetClient(ctx context.Context, in *clientpb.GetClientRequest, opts ...grpc.CallOption) (*clientpb.ClientResponse, error)
}

// WithEvents wires the Kafka producer so the service publishes its domain events
// itself (per CLAUDE.md, events are published from the service layer, not the
// handler). No-op publishing when nil. Returns the service for chaining.
func (s *AccountService) WithEvents(p eventPublisher) *AccountService {
	s.events = p
	return s
}

// WithClientLookup wires the client-service client used to fetch the owner's
// email for the account-created email. Returns the service for chaining.
func (s *AccountService) WithClientLookup(c clientLookup) *AccountService {
	s.clients = c
	return s
}

// hasHumanOwner reports whether the account belongs to a real client (not a
// bank/state-owned sentinel account), i.e. whether in-app/email notifications
// should be emitted.
func hasHumanOwner(a *model.Account) bool {
	return !a.IsBankAccount && a.OwnerID != BankOwnerID && a.OwnerID != StateOwnerID
}

// emitAccountCreated publishes the account-created domain event, plus (for
// client-owned accounts) an in-app notification and an activation/welcome email.
// Best-effort: the account write has already committed.
func (s *AccountService) emitAccountCreated(account *model.Account) {
	if s.events == nil {
		return
	}
	bg := context.Background()
	_ = s.events.PublishAccountCreated(bg, kafkamsg.AccountCreatedMessage{
		AccountNumber: account.AccountNumber,
		OwnerID:       account.OwnerID,
		AccountKind:   account.AccountKind,
		CurrencyCode:  account.CurrencyCode,
	})
	if !hasHumanOwner(account) {
		return
	}
	_ = s.events.PublishGeneralNotification(bg, kafkamsg.GeneralNotificationMessage{
		UserID:  account.OwnerID,
		Type:    "ACCOUNT_OPENED",
		Data:    map[string]string{"account_number": account.AccountNumber, "currency": account.CurrencyCode},
		RefType: "account",
		RefID:   account.ID,
	})
	// Welcome email — best effort; needs the owner's email from client-service.
	if s.clients == nil {
		return
	}
	ctx, cancel := context.WithTimeout(bg, emitTimeout)
	defer cancel()
	clientResp, err := s.clients.GetClient(ctx, &clientpb.GetClientRequest{Id: account.OwnerID})
	if err != nil {
		log.Printf("warn: fetch client %d for account-created email: %v", account.OwnerID, err)
		return
	}
	if err := s.events.SendEmail(bg, kafkamsg.SendEmailMessage{
		To:        clientResp.Email,
		EmailType: kafkamsg.EmailTypeAccountCreated,
		Data: map[string]string{
			"account_number": account.AccountNumber,
			"account_name":   account.AccountName,
			"currency":       account.CurrencyCode,
		},
	}); err != nil {
		log.Printf("warn: send account-created email to %s: %v", clientResp.Email, err)
	}
}

// emitAccountNameUpdated publishes the name-updated domain event (+ notification).
func (s *AccountService) emitAccountNameUpdated(account *model.Account, newName string) {
	if s.events == nil {
		return
	}
	bg := context.Background()
	_ = s.events.PublishAccountNameUpdated(bg, kafkamsg.AccountNameUpdatedMessage{
		AccountID:     account.ID,
		AccountNumber: account.AccountNumber,
		NewName:       newName,
	})
	if !hasHumanOwner(account) {
		return
	}
	_ = s.events.PublishGeneralNotification(bg, kafkamsg.GeneralNotificationMessage{
		UserID:  account.OwnerID,
		Type:    "ACCOUNT_NAME_UPDATED",
		Data:    map[string]string{"account_number": account.AccountNumber, "new_name": newName},
		RefType: "account",
		RefID:   account.ID,
	})
}

// emitAccountLimitsUpdated publishes the limits-updated domain event (+ notification).
func (s *AccountService) emitAccountLimitsUpdated(account *model.Account, daily, monthly string) {
	if s.events == nil {
		return
	}
	bg := context.Background()
	_ = s.events.PublishAccountLimitsUpdated(bg, kafkamsg.AccountLimitsUpdatedMessage{
		AccountID:     account.ID,
		AccountNumber: account.AccountNumber,
		DailyLimit:    daily,
		MonthlyLimit:  monthly,
	})
	if !hasHumanOwner(account) {
		return
	}
	_ = s.events.PublishGeneralNotification(bg, kafkamsg.GeneralNotificationMessage{
		UserID:  account.OwnerID,
		Type:    "ACCOUNT_LIMITS_UPDATED",
		Data:    map[string]string{"account_number": account.AccountNumber, "daily_limit": daily, "monthly_limit": monthly},
		RefType: "account",
		RefID:   account.ID,
	})
}

// emitAccountStatusChanged publishes the status-changed domain event (+ notification).
func (s *AccountService) emitAccountStatusChanged(account *model.Account, newStatus string) {
	if s.events == nil {
		return
	}
	bg := context.Background()
	_ = s.events.PublishAccountStatusChanged(bg, kafkaprod.AccountStatusChangedMsg{
		AccountNumber: account.AccountNumber,
		Status:        newStatus,
	})
	if !hasHumanOwner(account) {
		return
	}
	_ = s.events.PublishGeneralNotification(bg, kafkamsg.GeneralNotificationMessage{
		UserID:  account.OwnerID,
		Type:    "ACCOUNT_STATUS_CHANGED",
		Data:    map[string]string{"account_number": account.AccountNumber, "new_status": newStatus},
		RefType: "account",
		RefID:   account.ID,
	})
}
