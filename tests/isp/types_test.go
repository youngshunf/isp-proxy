//go:build ispe2e

package isp

import "time"

// 本文件是 api/openapi.yaml 中平台 E2E 用到的那部分类型（按契约手写，字段名与 JSON 一致）。
// 只用于解析响应与构造请求；契约本身由 internal/control/api 的契约测试校验。

type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

func CNY(cents int64) Money { return Money{Amount: cents, Currency: "CNY"} }

type Ports struct {
	HTTP   int `json:"http"`
	HTTPS  int `json:"https"`
	SOCKS5 int `json:"socks5"`
}

type Account struct {
	Email    string    `json:"email"`
	Verified bool      `json:"verified"`
	Status   string    `json:"status"`
	Created  time.Time `json:"createdAt"`
}

// ---- 用户视图：订阅 ----

type Exit struct {
	Version        int        `json:"version"`
	IP             string     `json:"ip"`
	CountryCode    string     `json:"countryCode"`
	Check          string     `json:"check"`
	LatencyMs      *int       `json:"latencyMs"`
	CheckedAt      *time.Time `json:"checkedAt"`
	SharedCapacity *int       `json:"sharedCapacity"`
}

type SubscriptionPlan struct {
	Name     string `json:"name"`
	ExitMode string `json:"exitMode"`
}

type Access struct {
	Version       int       `json:"version"`
	Host          string    `json:"host"`
	Username      string    `json:"username"`
	Password      string    `json:"password"`
	Ports         Ports     `json:"ports"`
	IssuedAt      time.Time `json:"issuedAt"`
	UnseenChanges []string  `json:"unseenChanges"`
}

type SubscriptionTime struct {
	PeriodDays       int        `json:"periodDays"`
	StartedAt        *time.Time `json:"startedAt"`
	ExpiresAt        *time.Time `json:"expiresAt"`
	RemainingSeconds int64      `json:"remainingSeconds"`
	RetentionEndsAt  *time.Time `json:"retentionEndsAt"`
}

type SubscriptionTraffic struct {
	TotalBytes     int64 `json:"totalBytes"`
	UsedBytes      int64 `json:"usedBytes"`
	RemainingBytes int64 `json:"remainingBytes"` // 仅管理端
}

type SubscriptionQuotas struct {
	AccessChangesLeft    int        `json:"accessChangesLeft"`
	AccessChangesPerDay  int        `json:"accessChangesPerDay"`
	FreeExitChangesLeft  int        `json:"freeExitChangesLeft"`
	FreeExitWindowEndsAt *time.Time `json:"freeExitWindowEndsAt"`
}

// SubscriptionPlan 是套餐快照（直出订阅为 null）。
type SubscriptionPlan struct {
	Name          string `json:"name"`
	CountryCode   string `json:"countryCode"`
	Concurrency   int    `json:"concurrency"`
	BandwidthMbps int    `json:"bandwidthMbps"`
}

// SubscriptionDirect 是直出代理信息（D-25）。
type SubscriptionDirect struct {
	BandwidthMbps  int        `json:"bandwidthMbps"`
	MaxConnections int        `json:"maxConnections"`
	PaidUntil      *time.Time `json:"paidUntil"`
}

// AdminUserDirect 是用户详情里的直出代理赠送状态。
type AdminUserDirect struct {
	SubscriptionID       *string `json:"subscriptionId"`
	Lifecycle            *string `json:"lifecycle"`
	BandwidthMbps        *int    `json:"bandwidthMbps"`
	DefaultBandwidthMbps int     `json:"defaultBandwidthMbps"`
	MaxConnections       int     `json:"maxConnections"`
}

type Subscription struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Kind              string              `json:"kind"`
	Plan              *SubscriptionPlan   `json:"plan"`
	Direct            *SubscriptionDirect `json:"direct"`
	Lifecycle         string              `json:"lifecycle"`
	Blockers          []string            `json:"blockers"`
	Time              SubscriptionTime    `json:"time"`
	Traffic           SubscriptionTraffic `json:"traffic"`
	Exit              *Exit               `json:"exit"`
	Access            *Access             `json:"access"`
	Quotas            SubscriptionQuotas  `json:"quotas"`
	OpenExitRequestID *string             `json:"openExitRequestId"`
	OpenOrderIDs      []string            `json:"openOrderIds"`
}

func (s *Subscription) Has(blocker string) bool {
	for _, b := range s.Blockers {
		if b == blocker {
			return true
		}
	}
	return false
}

type SubscriptionEvent struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Detail  string    `json:"detail"`
	OrderNo string    `json:"orderNo"`
}

// ---- 目录与订单 ----

type Plan struct {
	ID            string `json:"id"`
	VersionID     string `json:"versionId"`
	CountryCode   string `json:"countryCode"`
	City          string `json:"city"`
	MonthlyPrice  Money  `json:"monthlyPrice"`
	IncludedBytes int64  `json:"includedBytes"`
	Concurrency   int    `json:"concurrency"`
	BandwidthMbps int    `json:"bandwidthMbps"`
	ExitMode      string `json:"exitMode"`
	Stock         string `json:"stock"`
}

type TrafficPack struct {
	ID        string `json:"id"`
	VersionID string `json:"versionId"`
	Bytes     int64  `json:"bytes"`
	Price     Money  `json:"price"`
}

type PaymentInstructions struct {
	Payee   string `json:"payee"`
	Bank    string `json:"bank"`
	Account string `json:"account"`
	Note    string `json:"note"`
}

type Catalog struct {
	Plans        []Plan              `json:"plans"`
	TrafficPacks []TrafficPack       `json:"trafficPacks"`
	MonthOptions []int               `json:"monthOptions"`
	Payment      PaymentInstructions `json:"payment"`
	Rules        struct {
		Version         string `json:"version"`
		RetentionDays   int    `json:"retentionDays"`
		UnpaidHoldHours int    `json:"unpaidHoldHours"`
	} `json:"rules"`
}

type OrderLine struct {
	Label  string `json:"label"`
	Detail string `json:"detail"`
	Amount Money  `json:"amount"`
}

type Order struct {
	ID                string      `json:"id"`
	No                string      `json:"no"`
	Kind              string      `json:"kind"`
	CreatedAt         time.Time   `json:"createdAt"`
	HoldUntil         *time.Time  `json:"holdUntil"`
	Amount            Money       `json:"amount"`
	Payment           string      `json:"payment"`
	Fulfillment       string      `json:"fulfillment"`
	UserClaimedPaidAt *time.Time  `json:"userClaimedPaidAt"`
	PayRef            string      `json:"payRef"`
	SubscriptionID    *string     `json:"subscriptionId"`
	Lines             []OrderLine `json:"lines"`
	RulesSnapshot     []string    `json:"rulesSnapshot"`

	// 仅管理端（AdminOrder）
	PaymentRecord    *PaymentRecord `json:"paymentRecord"`
	LedgerEntries    []LedgerEntry  `json:"ledgerEntries"`
	Refunds          []Refund       `json:"refunds"`
	RulesVersion     string         `json:"rulesVersion"`
	PlanVersionID    *string        `json:"planVersionId"`
	FulfillmentError *string        `json:"fulfillmentError"`
	CancelReason     *string        `json:"cancelReason"`
}

type OrderInput struct {
	Kind           string `json:"kind"`
	PlanID         string `json:"planId,omitempty"`
	PlanVersionID  string `json:"planVersionId,omitempty"`
	Months         int    `json:"months,omitempty"`
	Quantity       int    `json:"quantity,omitempty"`
	SubscriptionID string `json:"subscriptionId,omitempty"`
	PackID         string `json:"packId,omitempty"`
	PackVersionID  string `json:"packVersionId,omitempty"`
}

type ExitRequest struct {
	ID               string  `json:"id"`
	No               string  `json:"no"`
	SubscriptionID   string  `json:"subscriptionId"`
	Reason           string  `json:"reason"`
	AlsoChangeAccess bool    `json:"alsoChangeAccess"`
	Charge           string  `json:"charge"`
	Status           string  `json:"status"`
	OldIP            string  `json:"oldIp"`
	NewIP            *string `json:"newIp"`
	FailureReason    *string `json:"failureReason"`
	// 管理端
	FeeStatus string  `json:"feeStatus"`
	TaskID    *string `json:"taskId"`
}

// ---- 管理端 ----

type AdminLoginChallenge struct {
	Stage      string `json:"stage"`
	Enrollment *struct {
		Secret     string `json:"secret"`
		OTPAuthURI string `json:"otpauthUri"`
	} `json:"enrollment"`
}

type AdminInfo struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	Role         string `json:"role"`
	TOTPEnrolled bool   `json:"totpEnrolled"`
}

type AdminSession struct {
	Admin         AdminInfo `json:"admin"`
	RecoveryCodes []string  `json:"recoveryCodes"`
}

type UserRef struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type AdminExit struct {
	BindingID        string  `json:"bindingId"`
	Version          int     `json:"version"`
	ResourceID       string  `json:"resourceId"`
	UpstreamProtocol string  `json:"upstreamProtocol"`
	UpstreamAddr     string  `json:"upstreamAddr"`
	VerifiedIP       *string `json:"verifiedIp"`
	IP               *string `json:"ip"`
	Check            *string `json:"check"`
}

type AdminAccess struct {
	ID       string `json:"id"`
	Version  int    `json:"version"`
	State    string `json:"state"`
	Host     string `json:"host"`
	Username string `json:"username"`
	Ports    Ports  `json:"ports"`
}

type AdminSubscription struct {
	ID                 string              `json:"id"`
	Name               string              `json:"name"`
	Kind               string              `json:"kind"`
	Direct             *SubscriptionDirect `json:"direct"`
	User               UserRef             `json:"user"`
	NodeID             *string             `json:"nodeId"`
	Lifecycle          string              `json:"lifecycle"`
	Blockers           []string            `json:"blockers"`
	AdminPaused        bool                `json:"adminPaused"`
	Time               SubscriptionTime    `json:"time"`
	Traffic            SubscriptionTraffic `json:"traffic"`
	Exit               *AdminExit          `json:"exit"`
	Access             *AdminAccess        `json:"access"`
	ActiveConns        *int                `json:"activeConns"`
	UsageReportedAt    *time.Time          `json:"usageReportedAt"`
	DesiredVersion     int64               `json:"desiredVersion"`
	AppliedVersion     int64               `json:"appliedVersion"`
	ActiveTaskID       *string             `json:"activeTaskId"`
	Quotas             SubscriptionQuotas  `json:"quotas"`
	RulesVersion       string              `json:"rulesVersion"`
	ReleasedAt         *time.Time          `json:"releasedAt"`
	CreatedFromOrderID *string             `json:"createdFromOrderId"`
	Plan               struct {
		PlanID        string `json:"planId"`
		PlanVersionID string `json:"planVersionId"`
		PlanVersion   int    `json:"planVersion"`
		Concurrency   int    `json:"concurrency"`
		BandwidthMbps int    `json:"bandwidthMbps"`
	} `json:"plan"`
}

func (s *AdminSubscription) Has(blocker string) bool {
	for _, b := range s.Blockers {
		if b == blocker {
			return true
		}
	}
	return false
}

type AdminCreateSubscriptionResult struct {
	Order        Order             `json:"order"`
	Subscription AdminSubscription `json:"subscription"`
	Task         *Task             `json:"task"`
}

type ConfirmPaymentInput struct {
	Method      string `json:"method"`
	Amount      Money  `json:"amount"`
	ExternalRef string `json:"externalRef"`
	Note        string `json:"note,omitempty"`
	LateArrival bool   `json:"lateArrival,omitempty"`
}

type PaymentRecord struct {
	ID          string `json:"id"`
	Amount      Money  `json:"amount"`
	Method      string `json:"method"`
	ExternalRef string `json:"externalRef"`
	LateArrival bool   `json:"lateArrival"`
}

type LedgerEntry struct {
	ID              string    `json:"id"`
	SubscriptionID  string    `json:"subscriptionId"`
	EntitlementKind string    `json:"entitlementKind"`
	Amount          int64     `json:"amount"`
	Unit            string    `json:"unit"`
	SourceType      string    `json:"sourceType"`
	SourceID        string    `json:"sourceId"`
	OrderID         *string   `json:"orderId"`
	RefundID        *string   `json:"refundId"`
	TransferID      *string   `json:"transferId"`
	Reason          string    `json:"reason"`
	CreatedAt       time.Time `json:"createdAt"`
}

type Ledger struct {
	Entries              []LedgerEntry `json:"entries"`
	EntitledBytesTotal   int64         `json:"entitledBytesTotal"`
	UsedBytesTotal       int64         `json:"usedBytesTotal"`
	EntitledSecondsTotal int64         `json:"entitledSecondsTotal"`
}

type Refund struct {
	ID             string `json:"id"`
	OrderID        string `json:"orderId"`
	Status         string `json:"status"`
	Amount         Money  `json:"amount"`
	ReclaimBytes   int64  `json:"reclaimBytes"`
	ReclaimSeconds int64  `json:"reclaimSeconds"`
}

type Transfer struct {
	ID                 string `json:"id"`
	UserID             string `json:"userId"`
	FromSubscriptionID string `json:"fromSubscriptionId"`
	ToSubscriptionID   string `json:"toSubscriptionId"`
	Bytes              int64  `json:"bytes"`
}

type Task struct {
	ID               string  `json:"id"`
	SubscriptionID   string  `json:"subscriptionId"`
	Kind             string  `json:"kind"`
	State            string  `json:"state"`
	NewAccessVersion *int    `json:"newAccessVersion"`
	NewExitVersion   *int    `json:"newExitVersion"`
	TargetVersion    *int64  `json:"targetVersion"`
	FailureCode      *string `json:"failureCode"`
	FailureReason    *string `json:"failureReason"`
}

func (t *Task) Done() bool {
	return t.State == "succeeded" || t.State == "failed" || t.State == "cancelled"
}

type ExitComparison struct {
	CurrentIP          *string `json:"currentIp"`
	DiffersFromCurrent bool    `json:"differsFromCurrent"`
	UniqueOnPlatform   bool    `json:"uniqueOnPlatform"`
	CountryMatches     bool    `json:"countryMatches"`
	Passed             bool    `json:"passed"`
}

type Probe struct {
	ID                  string          `json:"id"`
	Kind                string          `json:"kind"`
	Status              string          `json:"status"`
	Result              *string         `json:"result"`
	ObservedIP          *string         `json:"observedIp"`
	ObservedCountryCode *string         `json:"observedCountryCode"`
	Error               *string         `json:"error"`
	Comparison          *ExitComparison `json:"comparison"`
}

type Upstream struct {
	ID         string  `json:"id"`
	NodeID     string  `json:"nodeId"`
	Protocol   string  `json:"protocol"`
	Host       string  `json:"host"`
	Port       int     `json:"port"`
	Status     string  `json:"status"`
	BoundSub   *string `json:"boundSubscriptionId"`
	RowVersion int64   `json:"rowVersion"`
	MaxTenants int     `json:"maxTenants"`
	Occupancy  struct {
		Mode     string `json:"mode"`
		Used     int    `json:"used"`
		Bound    int    `json:"bound"`
		Reserved int    `json:"reserved"`
		Cooldown int    `json:"cooldown"`
	} `json:"occupancy"`
	// Tenants 只在详情接口返回（受影响订阅）。
	Tenants []struct {
		SubscriptionID string `json:"subscriptionId"`
		ExitMode       string `json:"exitMode"`
		BindingState   string `json:"bindingState"`
	} `json:"tenants"`
	LastVerify *struct {
		OK     bool    `json:"ok"`
		ExitIP *string `json:"exitIp"`
		Error  *string `json:"error"`
	} `json:"lastVerify"`
}

type UpstreamInput struct {
	Protocol    string `json:"protocol"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	NodeID      string `json:"nodeId"`
	CountryCode string `json:"countryCode"`
	Country     string `json:"country"`
	City        string `json:"city"`
	Provider    string `json:"provider"`
	MaxTenants  *int   `json:"maxTenants,omitempty"`
}

type PlanVersionInput struct {
	Name          string `json:"name"`
	CountryCode   string `json:"countryCode"`
	Country       string `json:"country"`
	City          string `json:"city"`
	Price         Money  `json:"price"`
	PeriodDays    int    `json:"periodDays"`
	IncludedBytes int64  `json:"includedBytes"`
	Concurrency   int    `json:"concurrency"`
	BandwidthMbps int    `json:"bandwidthMbps"`
}

type AdminPlan struct {
	ID       string `json:"id"`
	Code     string `json:"code"`
	Status   string `json:"status"`
	ExitMode string `json:"exitMode"`
	Current  struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
		Price   Money  `json:"price"`
	} `json:"current"`
	Stock struct {
		Available int    `json:"available"`
		Reserved  int    `json:"reserved"`
		Bound     int    `json:"bound"`
		Level     string `json:"level"`
	} `json:"stock"`
}

type AdminPack struct {
	ID      string `json:"id"`
	Code    string `json:"code"`
	Current struct {
		ID    string `json:"id"`
		Bytes int64  `json:"bytes"`
	} `json:"current"`
}

type RulesInput struct {
	Version                  string              `json:"version"`
	EffectiveFrom            time.Time           `json:"effectiveFrom"`
	PeriodDays               int                 `json:"periodDays"`
	RetentionDays            int                 `json:"retentionDays"`
	PortCooldownDays         int                 `json:"portCooldownDays"`
	ResourceCooldownDays     int                 `json:"resourceCooldownDays"`
	AccessChangesPerWindow   int                 `json:"accessChangesPerWindow"`
	AccessChangeWindowHours  int                 `json:"accessChangeWindowHours"`
	FreeExitChangesPerWindow int                 `json:"freeExitChangesPerWindow"`
	ExitChangeWindowDays     int                 `json:"exitChangeWindowDays"`
	UnpaidHoldHours          int                 `json:"unpaidHoldHours"`
	MaxUnpaidNewOrders       int                 `json:"maxUnpaidNewOrders"`
	MonthOptions             []int               `json:"monthOptions"`
	Payment                  PaymentInstructions `json:"payment"`
	SnapshotLines            []string            `json:"snapshotLines"`
}

type Node struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Online          bool       `json:"online"`
	Status          string     `json:"status"`
	LastReportAt    *time.Time `json:"lastReportAt"`
	PersistOK       *bool      `json:"persistOk"`
	DesiredRevision int64      `json:"desiredRevision"`
	AppliedRevision int64      `json:"appliedRevision"`
	ActiveConns     *int       `json:"activeConns"`
}

type AdminUser struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Verified bool   `json:"verified"`
	Status   string `json:"status"`
}

type Alert struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	Severity       string  `json:"severity"`
	Status         string  `json:"status"`
	Message        string  `json:"message"`
	NodeID         *string `json:"nodeId"`
	SubscriptionID *string `json:"subscriptionId"`
	ResourceID     *string `json:"resourceId"`
	OrderID        *string `json:"orderId"`
}

type UsagePoint struct {
	At        time.Time  `json:"at"`
	Bytes     *int64     `json:"bytes"`
	MaxConns  *int       `json:"maxConns"`
	Samples   int        `json:"samples"`
	SampledAt *time.Time `json:"sampledAt"`
}

type UsageSeries struct {
	SubscriptionID string       `json:"subscriptionId"`
	BucketSeconds  int          `json:"bucketSeconds"`
	LastSampleAt   *time.Time   `json:"lastSampleAt"`
	Freshness      string       `json:"freshness"`
	Points         []UsagePoint `json:"points"`
}

type Overview struct {
	DataAt  *time.Time `json:"dataAt"`
	Traffic struct {
		CurrentConns *int   `json:"currentConns"`
		RateBps      *int64 `json:"rateBps"`
	} `json:"traffic"`
}

type AuditLog struct {
	ID       int64          `json:"id"`
	Action   string         `json:"action"`
	Outcome  string         `json:"outcome"`
	TargetID *string        `json:"targetId"`
	Details  map[string]any `json:"details"`
}

type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}
