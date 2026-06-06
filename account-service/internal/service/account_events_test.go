package service

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"gorm.io/gorm"

	kafkaprod "github.com/exbanka/account-service/internal/kafka"
	"github.com/exbanka/account-service/internal/model"
	"github.com/exbanka/account-service/internal/repository"
	clientpb "github.com/exbanka/contract/clientpb"
	kafkamsg "github.com/exbanka/contract/kafka"
)

// stubEvents captures everything published so tests can assert the service layer
// (not the handler) now owns event publishing.
type stubEvents struct {
	created []kafkamsg.AccountCreatedMessage
	status  []kafkaprod.AccountStatusChangedMsg
	name    []kafkamsg.AccountNameUpdatedMessage
	limits  []kafkamsg.AccountLimitsUpdatedMessage
	notifs  []kafkamsg.GeneralNotificationMessage
	emails  []kafkamsg.SendEmailMessage
}

func (s *stubEvents) PublishAccountCreated(_ context.Context, m kafkamsg.AccountCreatedMessage) error {
	s.created = append(s.created, m)
	return nil
}
func (s *stubEvents) PublishAccountStatusChanged(_ context.Context, m kafkaprod.AccountStatusChangedMsg) error {
	s.status = append(s.status, m)
	return nil
}
func (s *stubEvents) PublishAccountNameUpdated(_ context.Context, m kafkamsg.AccountNameUpdatedMessage) error {
	s.name = append(s.name, m)
	return nil
}
func (s *stubEvents) PublishAccountLimitsUpdated(_ context.Context, m kafkamsg.AccountLimitsUpdatedMessage) error {
	s.limits = append(s.limits, m)
	return nil
}
func (s *stubEvents) PublishGeneralNotification(_ context.Context, m kafkamsg.GeneralNotificationMessage) error {
	s.notifs = append(s.notifs, m)
	return nil
}
func (s *stubEvents) SendEmail(_ context.Context, m kafkamsg.SendEmailMessage) error {
	s.emails = append(s.emails, m)
	return nil
}

type stubClients struct{ email string }

func (s stubClients) GetClient(_ context.Context, in *clientpb.GetClientRequest, _ ...grpc.CallOption) (*clientpb.ClientResponse, error) {
	return &clientpb.ClientResponse{Id: in.Id, Email: s.email}, nil
}

// newEventsService wires a real AccountService with stub events + client lookup.
func newEventsService(t *testing.T) (*AccountService, *gorm.DB, *stubEvents) {
	t.Helper()
	db := newTestDB(t)
	repo := repository.NewAccountRepository(db)
	ev := &stubEvents{}
	svc := NewAccountService(repo, db, nil).WithEvents(ev).WithClientLookup(stubClients{email: "owner@example.com"})
	return svc, db, ev
}

func TestEmit_CreateAccount_HumanOwner(t *testing.T) {
	svc, _, ev := newEventsService(t)

	acct := &model.Account{OwnerID: 1, CurrencyCode: "RSD", AccountKind: "current", AccountType: "standard", AccountName: "Checking"}
	require.NoError(t, svc.CreateAccount(acct))

	assert.Len(t, ev.created, 1, "AccountCreated published from the service")
	assert.Len(t, ev.notifs, 1, "ACCOUNT_OPENED notification for a human owner")
	require.Len(t, ev.emails, 1, "welcome email sent to the resolved owner address")
	assert.Equal(t, "owner@example.com", ev.emails[0].To)
}

func TestEmit_CreateBankAccount_NoHumanNotification(t *testing.T) {
	svc, _, ev := newEventsService(t)

	_, err := svc.CreateBankAccount("EUR", "foreign", "Bank EUR", decimal.Zero)
	require.NoError(t, err)

	assert.Len(t, ev.created, 1, "domain event still fires for bank-owned accounts")
	assert.Empty(t, ev.notifs, "no in-app notification for bank-owned accounts")
	assert.Empty(t, ev.emails, "no email for bank-owned accounts")
}

func TestEmit_UpdateAccountName(t *testing.T) {
	svc, db, ev := newEventsService(t)
	acct := seedAccount(t, db, "111000100000010011", decimal.NewFromInt(100), decimal.NewFromInt(1_000_000))

	require.NoError(t, svc.UpdateAccountName(acct.ID, acct.OwnerID, "Renamed", 0))

	require.Len(t, ev.name, 1)
	assert.Equal(t, "Renamed", ev.name[0].NewName)
	assert.Len(t, ev.notifs, 1, "name-updated notification for a human owner")
}

func TestEmit_UpdateAccountName_BankOwned_NoNotification(t *testing.T) {
	svc, db, ev := newEventsService(t)
	acct := &model.Account{
		AccountNumber: "111000100000011011", OwnerID: BankOwnerID, IsBankAccount: true,
		CurrencyCode: "RSD", AccountKind: "current", AccountType: "bank", Status: "active",
		Balance: decimal.Zero, AvailableBalance: decimal.Zero, ExpiresAt: time.Now().AddDate(1, 0, 0), Version: 1,
	}
	require.NoError(t, db.Create(acct).Error)

	require.NoError(t, svc.UpdateAccountName(acct.ID, acct.OwnerID, "Bank Renamed", 0))
	assert.Len(t, ev.name, 1, "domain event still fires")
	assert.Empty(t, ev.notifs, "no in-app notification for bank-owned accounts")
}

func TestEmit_UpdateAccountLimits(t *testing.T) {
	svc, db, ev := newEventsService(t)
	acct := seedAccount(t, db, "111000100000012011", decimal.NewFromInt(100), decimal.NewFromInt(1_000_000))

	daily := "5000"
	require.NoError(t, svc.UpdateAccountLimits(acct.ID, &daily, nil, 0))

	require.Len(t, ev.limits, 1)
	assert.Equal(t, "5000.00", ev.limits[0].DailyLimit)
	assert.Len(t, ev.notifs, 1)
}

func TestEmit_UpdateAccountStatus(t *testing.T) {
	svc, db, ev := newEventsService(t)
	acct := seedAccount(t, db, "111000100000013011", decimal.NewFromInt(100), decimal.NewFromInt(1_000_000))

	require.NoError(t, svc.UpdateAccountStatus(acct.ID, "inactive", 0))

	require.Len(t, ev.status, 1)
	assert.Equal(t, "inactive", ev.status[0].Status)
	assert.Len(t, ev.notifs, 1)
}
