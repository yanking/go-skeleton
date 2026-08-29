package exchange

import (
	"reflect"
	"testing"
)

func TestEvent_ZeroValueMeansIgnorable(t *testing.T) {
	var e Event
	if e.Kline != nil || e.Snapshot != nil {
		t.Fatal("零值 Event 应表示「本帧无需处理」")
	}
}

func TestEvent_AllFieldsArePointers(t *testing.T) {
	// 验证 Event 的所有字段都是指针类型。
	// 这保证了「全 nil 即可忽略」的约定能自动成立：
	// 如果将来有人往 Event 加非指针字段，这个测试会立刻转红。
	eventType := reflect.TypeOf(Event{})
	for i := 0; i < eventType.NumField(); i++ {
		field := eventType.Field(i)
		if field.Type.Kind() != reflect.Ptr {
			t.Fatalf("Event.%s 必须是指针类型，当前是 %v", field.Name, field.Type.Kind())
		}
	}
}
