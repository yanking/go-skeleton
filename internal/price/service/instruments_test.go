package service

import (
	"context"
	"errors"
	"testing"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/internal/price/model"
)

// errMockUpsertAll、errMockInstrument 分别是 mockInstrumentRepo.UpsertAll 与
// mockExchange.Instruments 预设失败时返回的错误，仅用于测试断言比对。
var (
	errMockUpsertAll  = errors.New("mock upsert 标的失败")
	errMockInstrument = errors.New("mock 拉取交易对失败")
)

// mockInstrumentRepo 是 InstrumentRepo 的测试替身：upserted 累计全部
// UpsertAll 调用收到的行（跨调用累加，方便直接按总条数断言），
// markDelistedCalls 记录 MarkDelistedExcept 被调用的次数，keptSymbols 记录
// 最近一次调用收到的 keep 参数——nil 表示从未调用过，与「调用时传了个空
// 切片」（非 nil 长度 0）区分开。
type mockInstrumentRepo struct {
	upserted          []model.Instrument
	upsertErr         error
	markDelistedCalls int
	keptSymbols       []string
	markDelistedErr   error
}

func (r *mockInstrumentRepo) UpsertAll(ctx context.Context, rows []model.Instrument) error {
	if r.upsertErr != nil {
		return r.upsertErr
	}
	r.upserted = append(r.upserted, rows...)
	return nil
}

func (r *mockInstrumentRepo) MarkDelistedExcept(ctx context.Context, exchange, market string, keep []string) error {
	r.markDelistedCalls++
	r.keptSymbols = keep
	if r.markDelistedErr != nil {
		return r.markDelistedErr
	}
	return nil
}

// TestImportInstruments_MarksMissingAsDelisted 锚定核心行为：交易所本轮不再
// 返回的标的必须调 MarkDelistedExcept 标记下架，而不是留在库里当有效标的
// ——历史 K 线还引用着已下架的标的，不能删行。
func TestImportInstruments_MarksMissingAsDelisted(t *testing.T) {
	repo := &mockInstrumentRepo{}
	ex := &mockExchange{instruments: []exchange.Instrument{{NativeSymbol: "BTCUSDT", Trading: true}}}
	svc := New(Config{}, Deps{Instruments: repo, Exchanges: map[string]exchange.Exchange{"m": ex}}, testLogger())

	if err := svc.ImportInstruments(context.Background(), "m"); err != nil {
		t.Fatal(err)
	}
	if len(repo.upserted) != 1 {
		t.Errorf("upsert 条数 = %d, want 1", len(repo.upserted))
	}
	if repo.keptSymbols == nil {
		t.Fatal("必须调用 MarkDelistedExcept——交易所不再返回的标的要标下架而不是留着当有效")
	}
}

// TestImportInstruments_UnknownExchangeErrors 锚定装配期错误：ex 未在
// Deps.Exchanges 里配置时直接报错，不触碰任何仓储方法。
func TestImportInstruments_UnknownExchangeErrors(t *testing.T) {
	repo := &mockInstrumentRepo{}
	svc := New(Config{}, Deps{Instruments: repo, Exchanges: map[string]exchange.Exchange{}}, testLogger())

	if err := svc.ImportInstruments(context.Background(), "missing"); err == nil {
		t.Fatal("交易所未配置应报错，got nil")
	}
	if len(repo.upserted) != 0 || repo.markDelistedCalls != 0 {
		t.Errorf("交易所未配置时不该触碰仓储, upserted=%d markDelistedCalls=%d", len(repo.upserted), repo.markDelistedCalls)
	}
}

// TestImportInstruments_FetchFailurePropagatesError 锚定拉取失败的上抛：
// Instruments() 报错必须原样（挂 %w）上抛，且不写库、不标记下架——半份数据
// 不能当全量结果用。
func TestImportInstruments_FetchFailurePropagatesError(t *testing.T) {
	repo := &mockInstrumentRepo{}
	ex := &mockExchange{instrumentsErr: errMockInstrument}
	svc := New(Config{}, Deps{Instruments: repo, Exchanges: map[string]exchange.Exchange{"m": ex}}, testLogger())

	err := svc.ImportInstruments(context.Background(), "m")
	if !errors.Is(err, errMockInstrument) {
		t.Fatalf("err = %v, want 挂住 errMockInstrument", err)
	}
	if len(repo.upserted) != 0 || repo.markDelistedCalls != 0 {
		t.Errorf("拉取失败时不该触碰仓储, upserted=%d markDelistedCalls=%d", len(repo.upserted), repo.markDelistedCalls)
	}
}

// TestImportInstruments_UpsertFailureSkipsMarkDelisted 锚定写库失败时不得
// 继续标记下架：UpsertAll 都没成功，此时任何「不在 keep 里」的判断都建立在
// 没写进去的数据上，标记下架会把还没来得及覆盖的旧数据错误地打上下架标签。
func TestImportInstruments_UpsertFailureSkipsMarkDelisted(t *testing.T) {
	repo := &mockInstrumentRepo{upsertErr: errMockUpsertAll}
	ex := &mockExchange{instruments: []exchange.Instrument{{NativeSymbol: "BTCUSDT", Trading: true}}}
	svc := New(Config{}, Deps{Instruments: repo, Exchanges: map[string]exchange.Exchange{"m": ex}}, testLogger())

	err := svc.ImportInstruments(context.Background(), "m")
	if !errors.Is(err, errMockUpsertAll) {
		t.Fatalf("err = %v, want 挂住 errMockUpsertAll", err)
	}
	if repo.markDelistedCalls != 0 {
		t.Errorf("UpsertAll 失败后不该再调用 MarkDelistedExcept, 调用次数 = %d, want 0", repo.markDelistedCalls)
	}
}

// TestImportInstruments_EmptyResultMarksAllDelisted 锚定「本轮一个标的都没
// 拉到」的边界：keep 必须是非 nil 的空切片（而不是跳过调用），据 repo 层
// MarkDelistedExcept 的既有语义，空 keep 会把该交易所下全部标的标为下架，
// 这正是「交易所整体下线」场景下的预期行为。
func TestImportInstruments_EmptyResultMarksAllDelisted(t *testing.T) {
	repo := &mockInstrumentRepo{}
	ex := &mockExchange{instruments: nil}
	svc := New(Config{}, Deps{Instruments: repo, Exchanges: map[string]exchange.Exchange{"m": ex}}, testLogger())

	if err := svc.ImportInstruments(context.Background(), "m"); err != nil {
		t.Fatal(err)
	}
	if repo.markDelistedCalls != 1 {
		t.Fatalf("MarkDelistedExcept 调用次数 = %d, want 1", repo.markDelistedCalls)
	}
	if repo.keptSymbols == nil || len(repo.keptSymbols) != 0 {
		t.Errorf("keptSymbols = %v, want 非 nil 的空切片", repo.keptSymbols)
	}
}

// TestImportInstruments_MapsTradingFlagToStatus 锚定 Trading 到 Status 的
// 映射：可交易标记为交易中，不可交易标记为已下架——即便交易所在这一轮里仍然
// 返回了该标的（只是标了不可交易），也不该被当成有效标的写入。
func TestImportInstruments_MapsTradingFlagToStatus(t *testing.T) {
	repo := &mockInstrumentRepo{}
	ex := &mockExchange{instruments: []exchange.Instrument{
		{NativeSymbol: "BTCUSDT", Symbol: "BTC-USDT", Base: "BTC", Quote: "USDT", Trading: true},
		{NativeSymbol: "OLDUSDT", Symbol: "OLD-USDT", Base: "OLD", Quote: "USDT", Trading: false},
	}}
	svc := New(Config{}, Deps{Instruments: repo, Exchanges: map[string]exchange.Exchange{"m": ex}}, testLogger())

	if err := svc.ImportInstruments(context.Background(), "m"); err != nil {
		t.Fatal(err)
	}
	if len(repo.upserted) != 2 {
		t.Fatalf("upsert 条数 = %d, want 2", len(repo.upserted))
	}

	rowBySymbol := make(map[string]model.Instrument, len(repo.upserted))
	for _, row := range repo.upserted {
		if row.Exchange != "m" {
			t.Errorf("row.Exchange = %q, want %q", row.Exchange, "m")
		}
		rowBySymbol[row.NativeSymbol] = row
	}

	btc := rowBySymbol["BTCUSDT"]
	if btc.Status != model.InstrumentStatusTrading {
		t.Errorf("BTCUSDT 的 Status = %d, want InstrumentStatusTrading(%d)", btc.Status, model.InstrumentStatusTrading)
	}
	if btc.Symbol != "BTC-USDT" || btc.Base != "BTC" || btc.Quote != "USDT" {
		t.Errorf("BTCUSDT 的 Symbol/Base/Quote = %q/%q/%q, want BTC-USDT/BTC/USDT", btc.Symbol, btc.Base, btc.Quote)
	}

	old := rowBySymbol["OLDUSDT"]
	if old.Status != model.InstrumentStatusDelisted {
		t.Errorf("OLDUSDT 的 Status = %d, want InstrumentStatusDelisted(%d)", old.Status, model.InstrumentStatusDelisted)
	}
	if old.Symbol != "OLD-USDT" || old.Base != "OLD" || old.Quote != "USDT" {
		t.Errorf("OLDUSDT 的 Symbol/Base/Quote = %q/%q/%q, want OLD-USDT/OLD/USDT", old.Symbol, old.Base, old.Quote)
	}
}
