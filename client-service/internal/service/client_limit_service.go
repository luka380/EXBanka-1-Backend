package service

import (
	"context"
	"fmt"
	"log"

	kafkaprod "github.com/exbanka/client-service/internal/kafka"
	"github.com/exbanka/client-service/internal/model"
	"github.com/exbanka/contract/changelog"
	kafkamsg "github.com/exbanka/contract/kafka"
	userpb "github.com/exbanka/contract/userpb"
	"github.com/shopspring/decimal"
)

// ClientLimitRepo is the interface for client limit persistence.
type ClientLimitRepo interface {
	GetByClientID(clientID int64) (*model.ClientLimit, error)
	Upsert(limit *model.ClientLimit) error
}

// ClientEmailLookup resolves a client's email for the limit-change notification
// (SP5 D1). Optional — when nil, only the in-app notification is sent.
type ClientEmailLookup interface {
	GetEmailByID(clientID int64) (string, error)
}

// ClientLimitService manages client transaction limits.
type ClientLimitService struct {
	limitRepo     ClientLimitRepo
	userLimitSvc  userpb.EmployeeLimitServiceClient
	producer      *kafkaprod.Producer
	changelogRepo ChangelogRepo
	emailLookup   ClientEmailLookup // optional (SP5 D1)
}

// WithEmailLookup wires the client-email lookup used by the limit-change
// notification (SP5 D1). Optional.
func (s *ClientLimitService) WithEmailLookup(l ClientEmailLookup) *ClientLimitService {
	s.emailLookup = l
	return s
}

// NewClientLimitService constructs a ClientLimitService.
func NewClientLimitService(
	limitRepo ClientLimitRepo,
	userLimitSvc userpb.EmployeeLimitServiceClient,
	producer *kafkaprod.Producer,
	changelogRepo ...ChangelogRepo,
) *ClientLimitService {
	svc := &ClientLimitService{
		limitRepo:    limitRepo,
		userLimitSvc: userLimitSvc,
		producer:     producer,
	}
	if len(changelogRepo) > 0 {
		svc.changelogRepo = changelogRepo[0]
	}
	return svc
}

// GetClientLimits returns the limits for a client, using defaults if none set.
func (s *ClientLimitService) GetClientLimits(clientID int64) (*model.ClientLimit, error) {
	return s.limitRepo.GetByClientID(clientID)
}

// SetClientLimits sets the transaction limits for a client.
// The setting employee's limits must be >= the values being set.
func (s *ClientLimitService) SetClientLimits(ctx context.Context, limit model.ClientLimit, changedBy int64) (*model.ClientLimit, error) {
	// Fetch old limits for changelog.
	oldLimit, _ := s.limitRepo.GetByClientID(limit.ClientID)
	// Verify the employee's own limits authorize these client limits
	if s.userLimitSvc != nil {
		empLimits, err := s.userLimitSvc.GetEmployeeLimits(ctx, &userpb.EmployeeLimitRequest{
			EmployeeId: limit.SetByEmployee,
		})
		if err != nil {
			log.Printf("warn: SetClientLimits employee-lookup gRPC failed: %v", err)
			return nil, fmt.Errorf("SetClientLimits(employee=%d): %w", limit.SetByEmployee, ErrEmployeeLookupFailed)
		}

		maxClientDaily, err := decimal.NewFromString(empLimits.MaxClientDailyLimit)
		if err != nil {
			log.Printf("warn: SetClientLimits invalid max_client_daily_limit decimal %q: %v", empLimits.MaxClientDailyLimit, err)
			return nil, fmt.Errorf("SetClientLimits: %w", ErrInvalidEmployeeLimits)
		}
		maxClientMonthly, err := decimal.NewFromString(empLimits.MaxClientMonthlyLimit)
		if err != nil {
			log.Printf("warn: SetClientLimits invalid max_client_monthly_limit decimal %q: %v", empLimits.MaxClientMonthlyLimit, err)
			return nil, fmt.Errorf("SetClientLimits: %w", ErrInvalidEmployeeLimits)
		}

		if limit.DailyLimit.GreaterThan(maxClientDaily) {
			return nil, fmt.Errorf("SetClientLimits: daily limit %s exceeds employee authorization %s: %w",
				limit.DailyLimit.String(), maxClientDaily.String(), ErrLimitsExceedEmployee)
		}
		if limit.MonthlyLimit.GreaterThan(maxClientMonthly) {
			return nil, fmt.Errorf("SetClientLimits: monthly limit %s exceeds employee authorization %s: %w",
				limit.MonthlyLimit.String(), maxClientMonthly.String(), ErrLimitsExceedEmployee)
		}
	}

	if err := s.limitRepo.Upsert(&limit); err != nil {
		return nil, err
	}
	ClientLimitUpdatesTotal.Inc()

	result, err := s.limitRepo.GetByClientID(limit.ClientID)
	if err != nil {
		return nil, err
	}

	// Record changelog.
	if s.changelogRepo != nil && oldLimit != nil {
		entries := changelog.Diff("client_limit", limit.ClientID, changedBy, "", []changelog.FieldChange{
			{Field: "daily_limit", OldValue: oldLimit.DailyLimit.String(), NewValue: result.DailyLimit.String()},
			{Field: "monthly_limit", OldValue: oldLimit.MonthlyLimit.String(), NewValue: result.MonthlyLimit.String()},
			{Field: "transfer_limit", OldValue: oldLimit.TransferLimit.String(), NewValue: result.TransferLimit.String()},
		})
		if len(entries) > 0 {
			_ = s.changelogRepo.CreateBatch(entries)
		}
	}

	if s.producer != nil {
		if pubErr := s.producer.PublishClientLimitsUpdated(ctx, kafkamsg.ClientLimitsUpdatedMessage{
			ClientID:      limit.ClientID,
			SetByEmployee: limit.SetByEmployee,
			Action:        "set",
		}); pubErr != nil {
			log.Printf("warn: failed to publish client-limits-updated event: %v", pubErr)
		}

		// SP5 D1: notify the client (in-app + best-effort email) of the change.
		data := map[string]string{
			"daily_limit":    result.DailyLimit.String(),
			"monthly_limit":  result.MonthlyLimit.String(),
			"transfer_limit": result.TransferLimit.String(),
			"currency":       "RSD",
		}
		if nErr := s.producer.PublishGeneralNotification(ctx, kafkamsg.GeneralNotificationMessage{
			UserID:  uint64(limit.ClientID),
			Type:    "LIMIT_CHANGED",
			Data:    data,
			RefType: "client_limit",
			RefID:   uint64(limit.ClientID),
		}); nErr != nil {
			log.Printf("warn: failed to publish LIMIT_CHANGED notification: %v", nErr)
		}
		if s.emailLookup != nil {
			if email, eErr := s.emailLookup.GetEmailByID(limit.ClientID); eErr == nil && email != "" {
				_ = s.producer.SendEmail(ctx, kafkamsg.SendEmailMessage{
					To:        email,
					EmailType: kafkamsg.EmailType("LIMIT_CHANGED"),
					Data:      data,
				})
			}
		}
	}

	return result, nil
}
