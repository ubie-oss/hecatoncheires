import { test, expect, type Page } from '@playwright/test';
import { CaseListPage } from '../pages/CaseListPage';
import { CaseFormPage } from '../pages/CaseFormPage';
import { TEST_WORKSPACE_ID } from '../fixtures/testData';

// Linking an existing Slack channel needs Slack: the e2e server has no Slack,
// so the existing-channel option, the channel list and the CreateCase answer
// are rewritten here. The CreateCase answers reproduce what the server sends
// when it rejects the channel: a non-2xx status (graphqlErrorStatusMiddleware)
// with data: null and the code in extensions.

const CHANNELS = [
  { __typename: 'SlackChannelCandidate', id: 'C000000001', name: 'sample-channel', isPrivate: false },
];

async function routeExistingChannel(
  page: Page,
  createCase: { status: number; code?: string; message: string },
  calls: { candidates: number },
): Promise<void> {
  await page.route('**/graphql', async (route) => {
    const op = route.request().postDataJSON()?.operationName;
    if (op === 'GetFieldConfiguration') {
      const response = await route.fetch();
      const json = await response.json();
      json.data.fieldConfiguration.allowExistingSlackChannel = true;
      await route.fulfill({ response, json });
    } else if (op === 'GetExistingSlackChannelCandidates') {
      calls.candidates++;
      await route.fulfill({
        json: {
          data: {
            existingSlackChannelCandidates: {
              __typename: 'SlackChannelCandidateList',
              channels: CHANNELS,
              hasMore: false,
              fetchedAt: new Date().toISOString(),
            },
          },
        },
      });
    } else if (op === 'CreateCase') {
      const extensions = createCase.code ? { code: createCase.code } : {};
      await route.fulfill({
        status: createCase.status,
        json: { data: null, errors: [{ message: createCase.message, extensions }] },
      });
    } else {
      await route.continue();
    }
  });
}

test.describe('Case creation in an existing Slack channel', () => {
  for (const { status, code, message } of [
    { status: 409, code: 'SLACK_CHANNEL_ALREADY_LINKED', message: 'This channel is already linked to another case.' },
    { status: 400, code: 'SLACK_CHANNEL_NOT_LINKABLE', message: 'This channel cannot be used.' },
    { status: 500, code: 'SLACK_CHANNEL_JOIN_FAILED', message: 'The bot could not join this channel.' },
  ]) {
    test(`shows ${code} (HTTP ${status}) under the channel picker and reloads the channels`, async ({ page }) => {
      const calls = { candidates: 0 };
      await routeExistingChannel(page, { status, code, message: 'rejected' }, calls);

      const caseListPage = new CaseListPage(page);
      const caseFormPage = new CaseFormPage(page);
      await caseListPage.navigate(TEST_WORKSPACE_ID);
      await caseListPage.waitForTableLoad();
      await caseListPage.clickNewCaseButton();
      await caseFormPage.waitForFormVisible();
      await caseFormPage.fillTitle(`existing-channel ${code} ${Date.now()}`);
      await caseFormPage.fillCustomField('category', 'bug');
      await caseFormPage.selectExistingChannel('sample-channel');
      expect(calls.candidates).toBe(1);

      await page.getByTestId('case-submit-button').click();

      await expect(caseFormPage.getExistingChannelError()).toContainText(message);
      await expect(page.getByText(/Response not successful/)).toHaveCount(0);
      await expect.poll(() => calls.candidates).toBe(2);
      // The form stays open so another channel can be chosen.
      await expect(page.getByTestId('case-submit-button')).toBeVisible();
    });
  }

  test('shows the server message of an unclassified HTTP 500 instead of the status line', async ({ page }) => {
    const calls = { candidates: 0 };
    await routeExistingChannel(page, { status: 500, message: 'createCase: internal failure' }, calls);

    const caseListPage = new CaseListPage(page);
    const caseFormPage = new CaseFormPage(page);
    await caseListPage.navigate(TEST_WORKSPACE_ID);
    await caseListPage.waitForTableLoad();
    await caseListPage.clickNewCaseButton();
    await caseFormPage.waitForFormVisible();
    await caseFormPage.fillTitle(`existing-channel unclassified ${Date.now()}`);
    await caseFormPage.fillCustomField('category', 'bug');
    await caseFormPage.selectExistingChannel('sample-channel');

    await page.getByTestId('case-submit-button').click();

    await expect(page.getByText('createCase: internal failure')).toBeVisible();
    await expect(caseFormPage.getExistingChannelError()).toHaveCount(0);
  });
});
