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
		// mbedtls's memcheck target wraps ctest -D ExperimentalMemCheck
		// in a sed/ctest/tail/rm pipeline. The dashboard arg is no
		// longer at a `ctest ` prefix; substring match catches it.
		{"ctest -D ExperimentalMemCheck wrapped in sed pipeline",
			`sed -i.bak s+/usr/bin/valgrind+` + "`which valgrind`" + `+ DartConfiguration.tcl && ctest -O memcheck.log -D ExperimentalMemCheck && tail -n1 memcheck.log | grep 'Memory checking results:' > /dev/null`, true},
		{"ctest -D ExperimentalCoverage (Coverage variant)",
			`ctest -D ExperimentalCoverage`, true},
		// Scripted-dashboard form (newer cmake / brotli): ctest -DMODEL=<Dashboard>
		// -DACTIONS=... -S CMakeFiles/CTestScript.cmake. The -DMODEL= cache var
		// isn't the `-D <Dashboard>` arg, so it needs its own match.
		{"ctest scripted ExperimentalStart",
			`/usr/bin/ctest -C Debug -DMODEL=Experimental -DACTIONS=Start -S CMakeFiles/CTestScript.cmake -V`, true},
		{"ctest scripted ExperimentalBuild",
			`/usr/bin/ctest -C Debug -DMODEL=Experimental -DACTIONS=Build -S CMakeFiles/CTestScript.cmake -V`, true},
		{"ctest scripted NightlyMemoryCheck",
			`/usr/bin/ctest -C Debug -DMODEL=NightlyMemoryCheck -S CMakeFiles/CTestScript.cmake -V`, true},
		{"ctest scripted ContinuousStart",
			`/usr/bin/ctest -DMODEL=Continuous -DACTIONS=Start -S CMakeFiles/CTestScript.cmake`, true},
		// A user -D cache var that merely starts with MODEL= but isn't a
		// dashboard model must NOT be swept up.
		{"user -DMODEL=Foo passes through",
			`ctest -DMODEL=Foo -S user.cmake`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCMakeInternalCmd(tc.cmd); got != tc.want {
				t.Errorf("isCMakeInternalCmd(%q) = %v; want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestIsMakeDirOnlyCmd: a pure make_directory / mkdir command is recognized
// (and thus dropped), while a chained command that also does real work is not.
func TestIsMakeDirOnlyCmd(t *testing.T) {
	yes := []string{
		"/usr/bin/cmake -E make_directory /b/Debug/lib/ocaml/llvm",
		"mkdir -p Debug/lib/ocaml/llvm",
		"mkdir /b/x",
	}
	for _, c := range yes {
		if !isMakeDirOnlyCmd(c) {
			t.Errorf("isMakeDirOnlyCmd(%q) = false, want true", c)
		}
	}
	no := []string{
		"mkdir -p x && cp a b",                          // chained — real work
		"/usr/bin/cmake -E make_directory x && touch y", // chained
		"/usr/bin/cmake -E copy a b",                    // copy, not mkdir
		"python3 gen.py > out.h",                        // codegen
		"",
	}
	for _, c := range no {
		if isMakeDirOnlyCmd(c) {
			t.Errorf("isMakeDirOnlyCmd(%q) = true, want false", c)
		}
	}
}
