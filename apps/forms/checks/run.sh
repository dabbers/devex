#!/bin/sh
# The critical-path suite the platform runs before promoting a change.
#
# Not coverage -- consequences. These are the paths whose breakage costs the
# customer money or trust: a form that will not accept an answer, an answer
# that is not stored, an export that cannot be imported, an admin surface that
# serves without an identity, and a health check that lies.
#
# The platform runs this from the pinned upstream version of the app rather
# than from the installation's working tree, so a change cannot rewrite the
# gate it has to pass.
set -eu

: "${FORMS_TEST_DATABASE_URL:?the platform provides a scratch database for checks}"

exec go test ./... -count=1 -run 'TestCriticalPath|TestARequiredQuestionIsRequired|TestExportRoundTrips|TestImportRefusesToMerge|TestTheAdminSurfaceRefusesWithoutAnIdentity|TestHealthIsHonest|TestTheAgentSurfaceAnswers' -v
