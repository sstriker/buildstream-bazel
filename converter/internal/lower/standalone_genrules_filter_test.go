package lower

import "testing"

func TestIsCMakeInternalCmd(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"plain user cmd", "python3 gen.py out.h", false},
		{"raw ninja install (cd + abs paths)",
			`cd /tmp/build && /usr/bin/cmake -DCMAKE_INSTALL_COMPONENT="llvm-headers" -P /tmp/build/cmake_install.cmake`, true},
		{"normalised install (no cd, no /usr/bin)",
			`cmake -DCMAKE_INSTALL_COMPONENT="llvm-headers" -P cmake_install.cmake`, true},
		{"install with DCMAKE_INSTALL_DO_STRIP",
			`cmake -DCMAKE_INSTALL_COMPONENT="x" -DCMAKE_INSTALL_DO_STRIP=1 -P cmake_install.cmake`, true},
		{"install with DCMAKE_INSTALL_LOCAL_ONLY",
			`cmake -DCMAKE_INSTALL_LOCAL_ONLY=1 -P cmake_install.cmake`, true},
		{"bare install",
			`cmake -P cmake_install.cmake`, true},
		{"regen-during-build",
			`cmake --regenerate-during-build -S. -B.`, true},
		{"raw cpack",
			`cpack -G TGZ --config CPackConfig.cmake`, true},
		{"cpack chained with rpmbuild",
			`cpack -G TGZ --config CPackSourceConfig.cmake -B srpm/SOURCES && rpmbuild -bs --define '_topdir srpm' llvm.spec`, true},
		{"echo no-interactive dialog (escaped form)",
			`echo No\ interactive\ CMake\ dialog\ available.`, true},
		{"echo no-interactive dialog (plain form)",
			`echo No interactive CMake dialog available.`, true},
		{"genuine user echo passes through",
			`echo "hello"`, false},
		{"user cmake -P with non-install script passes through",
			`cmake -P myscript.cmake`, false},
		// ctest -D <Dashboard> — CDash dashboard submission edges.
		{"ctest -D Experimental",
			`ctest -D Experimental`, true},
		{"ctest -D Nightly",
			`ctest -D Nightly`, true},
		{"ctest -D Continuous",
			`ctest -D Continuous`, true},
		// User-written ctest invocations don't carry the -D
		// Dashboard arg shape.
		{"user ctest passes through",
			`ctest --output-on-failure`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCMakeInternalCmd(tc.cmd); got != tc.want {
				t.Errorf("isCMakeInternalCmd(%q) = %v; want %v", tc.cmd, got, tc.want)
			}
		})
	}
}
