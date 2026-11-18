#!/bin/sh
# The critical-path suite the platform runs before promoting a change.
#
# Not coverage -- consequences. These are the paths whose breakage costs the
# customer money or trust: a form that will not accept an answer, an answer
# that is not stored, an export that cannot be imported, an admin surface that
# serves without an identity, and a health check that lies.
#
# The platform runs this against a change's workspace, with DABBERZ_CHECK_URL
# pointing at its live preview and DABBERZ_CHECK_DATABASE_URL at a throwaway
# database the suite may empty. A change that edits this file is reported
# before anyone promotes it, because a result from a gate the change rewrote is
# not an independent one.
set -eu

: "${DABBERZ_CHECK_DATABASE_URL:?the platform provides a throwaway database for checks}"

# The suite's own name for it. The platform's name is the contract; this one
# keeps the tests runnable by hand against a database nobody minds losing.
FORMS_TEST_DATABASE_URL="$DABBERZ_CHECK_DATABASE_URL"
export FORMS_TEST_DATABASE_URL

exec go test ./... -count=1 -run 'TestCriticalPath|TestARequiredQuestionIsRequired|TestExportRoundTrips|TestImportRefusesToMerge|TestTheAdminSurfaceRefusesWithoutAnIdentity|TestHealthIsHonest|TestTheAgentSurfaceAnswers' -v
