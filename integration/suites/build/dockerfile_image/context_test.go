package common_test

import (
	"path/filepath"
	"runtime"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	"github.com/werf/werf/integration/pkg/utils"
)

var werfRepositoryDir string

func init() {
	var err error
	werfRepositoryDir, err = filepath.Abs("../../../../")
	if err != nil {
		panic(err)
	}
}

var _ = Describe("context", func() {
	BeforeEach(func() {
		SuiteData.WerfRepoWorktreeDir = filepath.Join(SuiteData.TestDirPath, "werf_repo_worktree")

		utils.RunSucceedCommand(
			SuiteData.TestDirPath,
			"git",
			"clone", werfRepositoryDir, SuiteData.WerfRepoWorktreeDir,
		)

		utils.RunSucceedCommand(
			SuiteData.WerfRepoWorktreeDir,
			"git",
			"checkout", "-b", "integration-context-test", "v1.0.10",
		)
	})

	AfterEach(func() {
		utils.RunSucceedCommand(
			SuiteData.WerfRepoWorktreeDir,
			SuiteData.WerfBinPath,
			"purge", "--force",
		)
	})

	type entry struct {
		prepareFixturesFunc   func()
		expectedWindowsDigest string
		expectedUnixDigest    string
		expectedDigest        string
	}

	var itBody = func(entry entry) {
		entry.prepareFixturesFunc()

		output, err := utils.RunCommand(
			SuiteData.WerfRepoWorktreeDir,
			SuiteData.WerfBinPath,
			"build", "--debug",
		)
		Ω(err).ShouldNot(HaveOccurred())

		if runtime.GOOS == "windows" && entry.expectedWindowsDigest != "" {
			Ω(string(output)).Should(ContainSubstring(entry.expectedWindowsDigest))
		} else if entry.expectedUnixDigest != "" {
			Ω(string(output)).Should(ContainSubstring(entry.expectedUnixDigest))
		} else {
			Ω(string(output)).Should(ContainSubstring(entry.expectedDigest))
		}
	}

	var _ = DescribeTable("checksum", itBody,
		Entry("base", entry{
			prepareFixturesFunc: func() {
				utils.CopyIn(utils.FixturePath("context", "base"), SuiteData.WerfRepoWorktreeDir)

				utils.RunSucceedCommand(
					SuiteData.WerfRepoWorktreeDir,
					"git",
					"add", "werf.yaml", ".dockerignore", "Dockerfile",
				)
				utils.RunSucceedCommand(
					SuiteData.WerfRepoWorktreeDir,
					"git",
					"commit", "-m", "+",
				)
			},
			expectedDigest: "26f6bd1d7de41678c4dcfae8a3785d9655ee6b13c16e4498abb43d0b",
		}),
		Entry("contextAdd", entry{
			prepareFixturesFunc: func() {
				utils.CopyIn(utils.FixturePath("context", "context_add_file"), SuiteData.WerfRepoWorktreeDir)

				utils.RunSucceedCommand(
					SuiteData.WerfRepoWorktreeDir,
					"git",
					"add", "werf.yaml", "werf-giterminism.yaml", ".dockerignore", "Dockerfile",
				)
				utils.RunSucceedCommand(
					SuiteData.WerfRepoWorktreeDir,
					"git",
					"commit", "-m", "+",
				)
			},
			expectedWindowsDigest: "b1c6be25d30d2de58df66e46dc8a328176cc2744dc3bfc2ae8d2917b",
			expectedUnixDigest:    "48a81bd49a6d299f78b463628ef6dd2436c2fce6736f2ad624b92e7f",
		}),
	)
})
