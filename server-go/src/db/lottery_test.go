package db

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// lotteryPrize builds a prize row the way Prisma would hand it to
// weightedRandomSelect. remain == nil means "unlimited".
func lotteryPrize(id int32, prizeType models.LotteryPrizeType, probability string, remain *int32) models.LotteryPrize {
	return models.LotteryPrize{
		ID:             id,
		Type:           prizeType,
		Probability:    mustDecimalRaw(probability),
		RemainQuantity: remain,
	}
}

// mustDecimalRaw builds a models.Decimal from its string form, panicking on a
// bad literal (test data only).
func mustDecimalRaw(value string) models.Decimal {
	parsed, err := models.NewDecimalFromString(value)
	if err != nil {
		panic(err)
	}
	return parsed
}

// The interval walk is an absolute-percentage rule over ALL prizes: exhausted
// prizes keep their slot, a hit inside an exhausted slot falls back to `nothing`,
// and a random above the total sum also returns `nothing`.
func TestLotteryWeightedRandomSelectAbsoluteIntervals(t *testing.T) {
	prizes := []models.LotteryPrize{
		lotteryPrize(1, models.LotteryPrizeTypePoints, "10", nil),
		lotteryPrize(2, models.LotteryPrizeTypeBalance, "20", nil),
		lotteryPrize(3, models.LotteryPrizeTypeCpu, "30", nil),
		lotteryPrize(4, models.LotteryPrizeTypeNothing, "40", nil),
	}

	original := LotteryRandomFloat
	defer func() { LotteryRandomFloat = original }()

	cases := []struct {
		name   string
		random float64
		wantID int32
	}{
		{"5% hits the first slot", 0.05, 1},
		{"exactly 10% is the next slot (strict <)", 0.10, 2},
		{"15% hits the second slot", 0.15, 2},
		{"30% is the third slot", 0.30, 3},
		{"45% still the third slot", 0.45, 3},
		{"exactly 60% overflows into nothing", 0.60, 4},
		{"99% returns the nothing prize", 0.99, 4},
	}
	for _, tc := range cases {
		LotteryRandomFloat = func() float64 { return tc.random }
		selected, err := LotteryWeightedRandomSelect(prizes)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", tc.name, err)
		}
		if selected.ID != tc.wantID {
			t.Errorf("%s: selected %d, want %d", tc.name, selected.ID, tc.wantID)
		}
	}
}

func TestLotteryWeightedRandomSelectEmptyPool(t *testing.T) {
	if _, err := LotteryWeightedRandomSelect(nil); !errors.Is(err, LotteryErrNoAvailablePrizes) {
		t.Fatalf("empty pool error = %v, want NO_AVAILABLE_PRIZES", err)
	}
}

func TestLotteryWeightedRandomSelectAllExhausted(t *testing.T) {
	prizes := []models.LotteryPrize{
		lotteryPrize(1, models.LotteryPrizeTypePoints, "50", int32Ptr(0)),
		lotteryPrize(2, models.LotteryPrizeTypeNothing, "50", int32Ptr(0)),
	}
	if _, err := LotteryWeightedRandomSelect(prizes); !errors.Is(err, LotteryErrNoAvailablePrizes) {
		t.Fatalf("all-exhausted error = %v, want NO_AVAILABLE_PRIZES", err)
	}
}

func TestLotteryWeightedRandomSelectExhaustedSlotFallsBackToNothing(t *testing.T) {
	other := int32(5)
	prizes := []models.LotteryPrize{
		lotteryPrize(1, models.LotteryPrizeTypePoints, "10", int32Ptr(0)), // still occupies [0,10)
		lotteryPrize(2, models.LotteryPrizeTypeBalance, "40", &other),
		lotteryPrize(3, models.LotteryPrizeTypeNothing, "50", nil),
	}

	original := LotteryRandomFloat
	defer func() { LotteryRandomFloat = original }()

	LotteryRandomFloat = func() float64 { return 0.05 } // inside the exhausted slot
	selected, err := LotteryWeightedRandomSelect(prizes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.ID != 3 {
		t.Errorf("exhausted slot selected %d, want the nothing prize 3", selected.ID)
	}

	LotteryRandomFloat = func() float64 { return 0.25 } // inside the available slot
	selected, err = LotteryWeightedRandomSelect(prizes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.ID != 2 {
		t.Errorf("available slot selected %d, want 2", selected.ID)
	}
}

func TestLotteryWeightedRandomSelectExhaustedWithoutNothingContinues(t *testing.T) {
	prizes := []models.LotteryPrize{
		lotteryPrize(1, models.LotteryPrizeTypePoints, "10", int32Ptr(0)),
		lotteryPrize(2, models.LotteryPrizeTypeBalance, "90", nil),
	}

	original := LotteryRandomFloat
	defer func() { LotteryRandomFloat = original }()
	LotteryRandomFloat = func() float64 { return 0.05 }

	selected, err := LotteryWeightedRandomSelect(prizes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.ID != 2 {
		t.Errorf("selected %d, want the next available prize 2", selected.ID)
	}
}

// A random above the configured sum must be rejected when no `nothing` prize
// exists: silently redistributing the remainder would change payout odds.
func TestLotteryWeightedRandomSelectConfigError(t *testing.T) {
	prizes := []models.LotteryPrize{
		lotteryPrize(1, models.LotteryPrizeTypePoints, "50", nil),
	}

	original := LotteryRandomFloat
	defer func() { LotteryRandomFloat = original }()
	LotteryRandomFloat = func() float64 { return 0.75 }

	if _, err := LotteryWeightedRandomSelect(prizes); !errors.Is(err, LotteryErrConfigError) {
		t.Fatalf("error = %v, want LOTTERY_CONFIG_ERROR", err)
	}
}

func TestLotteryNormalizePrizeType(t *testing.T) {
	if got := lotteryNormalizePrizeType(models.LotteryPrizeTypeTraffic); got != models.LotteryPrizeTypeTraffic {
		t.Errorf("valid type = %q", got)
	}
	if got := lotteryNormalizePrizeType("nope"); got != "" {
		t.Errorf("invalid type = %q, want empty", got)
	}
	if got := lotteryNormalizePrizeType(""); got != "" {
		t.Errorf("empty type = %q", got)
	}
}

func TestLotteryClampPagination(t *testing.T) {
	cases := []struct {
		page, pageSize         int
		wantPage, wantPageSize int
	}{
		{0, 0, 1, 20},
		{-3, -1, 1, 20},
		{2, 50, 2, 50},
		{1, 500, 1, 100},
		{100000, 1, 100000, 1},
	}
	for _, tc := range cases {
		page, pageSize := lotteryClampPagination(tc.page, tc.pageSize)
		if page != tc.wantPage || pageSize != tc.wantPageSize {
			t.Errorf("clamp(%d,%d) = (%d,%d), want (%d,%d)", tc.page, tc.pageSize, page, pageSize, tc.wantPage, tc.wantPageSize)
		}
	}
}

func TestLotteryPointsSafetyBounds(t *testing.T) {
	if !lotteryIsSafePointsAmount(1) || !lotteryIsSafePointsAmount(1_000_000_000) {
		t.Error("valid amounts rejected")
	}
	for _, value := range []float64{0, -1, 1.5, 1_000_000_001} {
		if lotteryIsSafePointsAmount(value) {
			t.Errorf("amount %v accepted", value)
		}
	}
	if !lotteryIsSafePointsBalance(0) || !lotteryIsSafePointsBalance(2_147_483_647) {
		t.Error("valid balances rejected")
	}
	if lotteryIsSafePointsBalance(-1) || lotteryIsSafePointsBalance(2_147_483_648) {
		t.Error("invalid balances accepted")
	}
}

func TestLotteryRetryableTransactionErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, true},
		{"message deadlock", errors.New("deadlock detected"), true},
		{"message write conflict", errors.New("write conflict"), true},
		{"business error", errors.New("积分不足"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := lotteryIsRetryableTransactionError(tc.err); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The draw result JSON keeps badgeOwnership/instanceDesc as explicit nulls, so the
// route response shape matches the TypeScript's object literal.
func TestLotteryDrawPrizeJSON(t *testing.T) {
	prize := LotteryDrawPrize{
		ID:    5,
		Name:  "谢谢参与",
		Type:  models.LotteryPrizeTypeNothing,
		Value: 0,
	}
	if prize.InstanceDesc != nil || prize.BadgeOwnership != nil {
		t.Fatal("zero prize must serialize explicit nulls via the route layer")
	}
}

func TestLotteryPrizeTypeSetMatchesPrismaEnum(t *testing.T) {
	expected := []models.LotteryPrizeType{
		models.LotteryPrizeTypeNothing,
		models.LotteryPrizeTypePoints,
		models.LotteryPrizeTypeBalance,
		models.LotteryPrizeTypeBadge,
		models.LotteryPrizeTypeInstance,
		models.LotteryPrizeTypeCpu,
		models.LotteryPrizeTypeMemory,
		models.LotteryPrizeTypeDisk,
		models.LotteryPrizeTypeTraffic,
	}
	if len(lotteryPrizeTypes) != len(expected) {
		t.Fatalf("lotteryPrizeTypes has %d entries, want %d", len(lotteryPrizeTypes), len(expected))
	}
	for _, prizeType := range expected {
		if !lotteryPrizeTypes[prizeType] {
			t.Errorf("missing prize type %q", prizeType)
		}
	}
}
