package releaseserver_test

import (
	"github.com/werf/werf/integration/pkg/utils"
	"github.com/werf/werf/integration/pkg/utils/liveexec"
)

func werfConverge(dir string, opts liveexec.ExecCommandOptions, extraArgs ...string) error {
	return liveexec.ExecCommand(dir, SuiteData.WerfBinPath, opts, utils.WerfBinArgs(append([]string{"converge"}, extraArgs...)...)...)
}
