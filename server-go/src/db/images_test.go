package db

import "testing"

func TestNormalizeImageArchitecture(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"aarch64", "aarch64"},
		{"ARM64", "aarch64"},
		{" arm64 ", "aarch64"},
		{"x86_64", "x86_64"},
		{"AMD64", "x86_64"},
		{"x64", "x86_64"},
		{"x86", "x86_64"},
		{"", "x86_64"},
		{"riscv64", "x86_64"},
	}

	for _, tc := range cases {
		if got := normalizeImageArchitecture(tc.input); got != tc.want {
			t.Errorf("normalizeImageArchitecture(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestImageInstanceTypeDefaults(t *testing.T) {
	if got := getImageInstanceType(""); got != imageTypeBoth {
		t.Errorf("getImageInstanceType(\"\") = %q, want %q", got, imageTypeBoth)
	}
	if got := getImageInstanceType(imageTypeVM); got != imageTypeVM {
		t.Errorf("getImageInstanceType(vm) = %q, want vm", got)
	}
	if got := getHostInstanceType(""); got != imageTypeContainer {
		t.Errorf("getHostInstanceType(\"\") = %q, want %q", got, imageTypeContainer)
	}
	if got := getHostInstanceType(imageTypeVM); got != imageTypeVM {
		t.Errorf("getHostInstanceType(vm) = %q, want vm", got)
	}
}

func TestIsImageCompatibleWithTargetType(t *testing.T) {
	cases := []struct {
		imageType  string
		targetType string
		want       bool
	}{
		{imageTypeBoth, imageTypeBoth, true},
		{imageTypeVM, imageTypeBoth, true},
		{imageTypeBoth, imageTypeVM, true},
		{imageTypeVM, imageTypeVM, true},
		{imageTypeContainer, imageTypeVM, false},
		{imageTypeVM, imageTypeContainer, false},
		{"weird", imageTypeBoth, true},
		{"weird", imageTypeVM, false},
	}

	for _, tc := range cases {
		if got := isImageCompatibleWithTargetType(tc.imageType, tc.targetType); got != tc.want {
			t.Errorf("isImageCompatibleWithTargetType(%q, %q) = %v, want %v",
				tc.imageType, tc.targetType, got, tc.want)
		}
	}
}

func TestIsImageCompatibleWithLowMemory(t *testing.T) {
	memory := func(value int) *int { return &value }

	if !isImageCompatibleWithLowMemory("ubuntu", nil) {
		t.Error("undefined memory must always be compatible")
	}
	if !isImageCompatibleWithLowMemory("ubuntu", memory(129)) {
		t.Error("memory above the 128MB threshold must always be compatible")
	}
	if !isImageCompatibleWithLowMemory("alpine", memory(128)) {
		t.Error("alpine must be allowed at 128MB")
	}
	if !isImageCompatibleWithLowMemory("Alpine", memory(64)) {
		t.Error("the low-memory distro check is case-insensitive")
	}
	if isImageCompatibleWithLowMemory("ubuntu", memory(128)) {
		t.Error("ubuntu must be rejected at 128MB")
	}
	if isImageCompatibleWithLowMemory("ubuntu", memory(0)) {
		t.Error("memory 0 is at or below the threshold, so only alpine/debian survive")
	}
	if !isImageCompatibleWithLowMemory("debian", memory(0)) {
		t.Error("debian must be allowed at memory 0")
	}
}

func TestNonNilImages(t *testing.T) {
	if got := nonNilImages(nil); got == nil || len(got) != 0 {
		t.Errorf("nonNilImages(nil) = %#v, want an empty non-nil slice", got)
	}
	images := nonNilImages(nil)
	if images == nil {
		t.Fatal("nonNilImages must never return nil (Prisma emits [] not null)")
	}
}
