import { Page, Locator } from '@playwright/test';
import { BasePage } from './BasePage';

/**
 * Page Object for the Case Detail page
 */
export class CaseDetailPage extends BasePage {
  // Locators
  private readonly caseTitle: Locator;
  private readonly caseDescription: Locator;
  private readonly deleteButton: Locator;
  private readonly backButton: Locator;
  private readonly loadingIndicator: Locator;
  private readonly slackChannelLink: Locator;
  private readonly fieldsSection: Locator;
  private readonly emptyActionState: Locator;

  constructor(page: Page) {
    super(page);
    // Case title is the inline-editable text inside the h1 wrapper
    this.caseTitle = page.getByTestId('case-title');
    // Description is the inline-editable long-text component
    this.caseDescription = page.getByTestId('case-description');
    // Delete moved into a kebab/hamburger menu
    this.deleteButton = page.getByTestId('case-delete-menu-item');
    this.backButton = page.locator('button, a').filter({ hasText: /Back/ }).first();
    this.loadingIndicator = page.locator('text=Loading...');
    // Slack channel link is now in the title row (an <a> tag starting with #)
    this.slackChannelLink = page.getByTestId('aside-slack-link');
    // The redesigned aside exposes a stable data-testid on the section.
    this.fieldsSection = page.getByTestId('case-fields-inline');
    // Empty action state title is now an h3 ("No actions yet") in the redesigned detail layout
    this.emptyActionState = page.locator('h3').filter({ hasText: 'No actions yet' }).first();
  }

  /**
   * Navigate to a specific case detail page
   */
  async navigate(workspaceId: string, caseId: number): Promise<void> {
    await this.goto(`/ws/${workspaceId}/cases/${caseId}`);
  }

  /**
   * Get the case title
   */
  async getTitle(): Promise<string> {
    return await this.caseTitle.textContent() || '';
  }

  /**
   * Get the case description
   */
  async getDescription(): Promise<string> {
    return await this.caseDescription.textContent() || '';
  }

  /**
   * Whether the "Test" badge is shown in the detail header.
   */
  async hasTestBadge(): Promise<boolean> {
    return await this.page.getByTestId('test-badge').isVisible();
  }

  /**
   * Whether the inline Test toggle is currently checked.
   */
  async isTestToggleChecked(): Promise<boolean> {
    return await this.page.getByTestId('test-case-toggle').isChecked();
  }

  /**
   * Flip the inline Test toggle to the desired state and wait for the
   * badge to reflect the change (the updateCase mutation refetches the
   * case, so the badge appears/disappears once it lands).
   */
  async setTestToggle(checked: boolean): Promise<void> {
    const toggle = this.page.getByTestId('test-case-toggle');
    await toggle.waitFor({ state: 'visible', timeout: 5000 });
    if ((await toggle.isChecked()) !== checked) {
      await toggle.click();
    }
    const badge = this.page.getByTestId('test-badge');
    if (checked) {
      await badge.waitFor({ state: 'visible', timeout: 10000 });
    } else {
      await badge.waitFor({ state: 'hidden', timeout: 10000 });
    }
  }

  /**
   * Inline-edit the case title (Linear-style: click → input → Enter).
   */
  async setTitle(next: string): Promise<void> {
    await this.caseTitle.click();
    const input = this.page.getByTestId('case-title-input');
    await input.waitFor({ state: 'visible', timeout: 3000 });
    await input.fill(next);
    await input.press('Enter');
  }

  /**
   * Inline-edit the case description (click → textarea → Save).
   */
  async setDescription(next: string): Promise<void> {
    await this.caseDescription.click();
    const input = this.page.getByTestId('case-description-input');
    await input.waitFor({ state: 'visible', timeout: 3000 });
    await input.fill(next);
    await this.page.getByTestId('case-description-save').click();
  }

  /**
   * Click the delete button (opens the kebab menu first, then the delete item)
   */
  async clickDelete(): Promise<void> {
    await this.page.getByTestId('case-menu-button').click();
    await this.deleteButton.waitFor({ state: 'visible', timeout: 3000 });
    await this.deleteButton.click();
  }

  /**
   * Archive the case (kebab menu → Archive). Only offered on a CLOSED,
   * non-archived case.
   */
  async clickArchive(): Promise<void> {
    await this.page.getByTestId('case-menu-button').click();
    const item = this.page.getByTestId('case-archive-menu-item');
    await item.waitFor({ state: 'visible', timeout: 3000 });
    const responsePromise = this.page.waitForResponse(
      (resp) => resp.url().includes('/graphql') && resp.status() === 200
    );
    await item.click();
    await responsePromise;
  }

  /**
   * Restore the case (kebab menu → Unarchive). Only offered on an archived case.
   */
  async clickUnarchive(): Promise<void> {
    await this.page.getByTestId('case-menu-button').click();
    const item = this.page.getByTestId('case-unarchive-menu-item');
    await item.waitFor({ state: 'visible', timeout: 3000 });
    const responsePromise = this.page.waitForResponse(
      (resp) => resp.url().includes('/graphql') && resp.status() === 200
    );
    await item.click();
    await responsePromise;
  }

  /**
   * The header's Archived badge. Returned as a Locator so callers assert with
   * Playwright's auto-retrying matchers: the badge appears once the mutation
   * response lands in the cache and React re-renders, which is after the
   * network response the click awaited.
   */
  archivedBadge(): Locator {
    return this.page.getByTestId('case-archived-badge');
  }

  /** The notice an archived (read-only) case shows above its body. */
  readOnlyNotice(): Locator {
    return this.page.getByTestId('case-read-only-notice');
  }

  /** The inline title editor's display frame (aria-disabled when read-only). */
  titleFrame(): Locator {
    return this.page.getByTestId('case-title');
  }

  /** A Related Actions row by its title. */
  relatedActionByTitle(title: string): Locator {
    return this.page.getByTestId('case-related-action').filter({ hasText: title });
  }

  /**
   * Whether the kebab menu offers the given archive action. Leaves the menu
   * open, so callers that go on to click it can reuse it.
   */
  async kebabOffers(action: 'archive' | 'unarchive'): Promise<boolean> {
    await this.page.getByTestId('case-menu-button').click();
    await this.page.getByTestId('case-menu-popover').waitFor({ state: 'visible', timeout: 3000 });
    return await this.page.getByTestId(`case-${action}-menu-item`).isVisible();
  }

  /**
   * Click the back button
   */
  async clickBack(): Promise<void> {
    await this.backButton.click();
    await this.waitForNavigation();
  }

  /**
   * Wait for the page to load
   */
  async waitForPageLoad(): Promise<void> {
    await this.loadingIndicator.waitFor({ state: 'hidden', timeout: 10000 }).catch(() => {});
    await this.caseTitle.waitFor({ state: 'visible', timeout: 10000 });
  }

  /**
   * Get a field value from the Fields grid by field label name
   */
  async getFieldValue(fieldName: string): Promise<string> {
    // Find the field item that contains a label div with the given name,
    // then get the sibling value div's text
    const fieldItem = this.page
      .locator('div')
      .filter({ has: this.page.locator('div').filter({ hasText: new RegExp(`^${fieldName}$`) }) })
      .first();
    const valueDiv = fieldItem.locator('div').nth(1);
    return await valueDiv.textContent() || '';
  }

  /**
   * Check if the Fields section is visible
   */
  async isFieldsSectionVisible(): Promise<boolean> {
    return await this.fieldsSection.isVisible();
  }

  /**
   * Check if the page is loaded
   */
  async isPageLoaded(): Promise<boolean> {
    try {
      await this.caseTitle.waitFor({ state: 'visible', timeout: 5000 });
      return true;
    } catch {
      return false;
    }
  }

  /**
   * Check if the Slack channel link is visible
   */
  async isSlackChannelButtonVisible(): Promise<boolean> {
    return await this.slackChannelLink.isVisible();
  }

  /**
   * Get the Slack channel link text
   */
  async getSlackChannelButtonText(): Promise<string> {
    return await this.slackChannelLink.textContent() || '';
  }

  /**
   * Get the Slack channel link href
   */
  async getSlackChannelButtonHref(): Promise<string> {
    return await this.slackChannelLink.getAttribute('href') || '';
  }

  /**
   * Get the current case status (Open/Closed)
   */
  async getStatus(): Promise<string> {
    const chip = this.page.locator('main').locator('span').filter({ hasText: /^(Open|Closed)$/ }).first();
    return await chip.textContent() || '';
  }

  /**
   * Check if timestamps section is visible
   */
  async isTimestampsVisible(): Promise<boolean> {
    return await this.page.getByTestId('created-timestamp-value').isVisible();
  }

  /**
   * Get the Created timestamp text
   */
  async getCreatedTimestamp(): Promise<string> {
    return await this.page.getByTestId('created-timestamp-value').textContent() || '';
  }

  /**
   * Get the Updated timestamp text
   */
  async getUpdatedTimestamp(): Promise<string> {
    return await this.page.getByTestId('updated-timestamp-value').textContent() || '';
  }

  /**
   * Machine-readable (ISO 8601) Created timestamp from the `data-ts` attribute.
   * Parsing this is locale-independent, unlike the localized display text.
   */
  async getCreatedTimestampISO(): Promise<string> {
    return (await this.page.getByTestId('created-timestamp-value').getAttribute('data-ts')) || '';
  }

  /** Machine-readable (ISO 8601) Updated timestamp from the `data-ts` attribute. */
  async getUpdatedTimestampISO(): Promise<string> {
    return (await this.page.getByTestId('updated-timestamp-value').getAttribute('data-ts')) || '';
  }

  /**
   * Check if the empty action state is visible
   */
  async isEmptyActionStateVisible(): Promise<boolean> {
    return await this.emptyActionState.isVisible();
  }

  /**
   * Click the Close button (opens confirmation modal and confirms)
   */
  async clickCloseButton(): Promise<void> {
    // Click the Close button in the header (scoped by data-testid) to open confirmation modal
    const closeBtn = this.page.getByTestId('close-case-button');
    await closeBtn.click();

    // Wait for the confirmation modal and click the confirm Close button (scoped by data-testid)
    const confirmBtn = this.page.getByTestId('confirm-close-button');
    await confirmBtn.waitFor({ state: 'visible', timeout: 5000 });
    const responsePromise = this.page.waitForResponse(
      (resp) => resp.url().includes('/graphql') && resp.status() === 200
    );
    await confirmBtn.click();
    await responsePromise;
  }

  /**
   * Click the Reopen button
   */
  async clickReopenButton(): Promise<void> {
    const reopenBtn = this.page.locator('button').filter({ hasText: /Reopen/ }).first();
    const responsePromise = this.page.waitForResponse(
      (resp) => resp.url().includes('/graphql') && resp.status() === 200
    );
    await reopenBtn.click();
    await responsePromise;
  }

  /**
   * Check if Close button is visible
   */
  async isCloseButtonVisible(): Promise<boolean> {
    const closeBtn = this.page.getByTestId('close-case-button');
    return await closeBtn.isVisible();
  }

  /**
   * Check if Reopen button is visible
   */
  async isReopenButtonVisible(): Promise<boolean> {
    const reopenBtn = this.page.locator('button').filter({ hasText: /Reopen/ }).first();
    return await reopenBtn.isVisible();
  }

  /**
   * Click the Add Action button in the empty state
   */
  async clickAddActionInEmptyState(): Promise<void> {
    const addBtn = this.page.locator('button').filter({ hasText: /Add Action/ }).first();
    await addBtn.click();
  }

  /**
   * Check if the Private badge is visible on case detail
   */
  async isPrivateBadgeVisible(): Promise<boolean> {
    return await this.page.getByTestId('private-badge').isVisible();
  }

  /**
   * Get the Private badge text
   */
  async getPrivateBadgeText(): Promise<string> {
    return (await this.page.getByTestId('private-badge').textContent()) || '';
  }

  /**
   * Check if the Channel Members section is visible
   */
  async isChannelMembersSectionVisible(): Promise<boolean> {
    return await this.page.getByTestId('channel-members-section').isVisible();
  }

  /**
   * Get the channel members section title text (includes count)
   */
  async getChannelMembersSectionTitle(): Promise<string> {
    const section = this.page.getByTestId('channel-members-section');
    // The redesigned aside renders the title as <span class="h-aside-title">…</span>
    const title = section.locator('.h-aside-title').first();
    return (await title.textContent()) || '';
  }

  /**
   * Click the Sync button in channel members section
   */
  async clickSyncButton(): Promise<void> {
    const syncBtn = this.page.getByTestId('sync-members-button');
    const responsePromise = this.page.waitForResponse(
      (resp) => resp.url().includes('/graphql') && resp.status() === 200
    );
    await syncBtn.click();
    await responsePromise;
  }

  /**
   * Check if Sync button is visible
   */
  async isSyncButtonVisible(): Promise<boolean> {
    return await this.page.getByTestId('sync-members-button').isVisible();
  }

  /**
   * Filter channel members by name
   */
  async filterMembers(text: string): Promise<void> {
    await this.page.getByTestId('member-search-filter').fill(text);
    await this.page.evaluate(() => new Promise(resolve => requestAnimationFrame(resolve)));
  }

  /**
   * Locator matching every rendered member row. Returned as a Locator (not a
   * count) so callers can use auto-retrying `expect(...).toHaveCount(n)` and
   * avoid racing the debounced member filter.
   */
  memberItems(): Locator {
    return this.page.getByTestId('channel-members-section').locator('[class*="memberItem"]');
  }

  /**
   * Get the number of member items currently displayed
   */
  async getMemberCount(): Promise<number> {
    const members = await this.memberItems().all();
    return members.length;
  }

  /**
   * Click the Edit button shown when the case is in DRAFT status. Opens
   * the CaseForm modal pre-populated with the draft's current values.
   */
  async clickEditDraft(): Promise<void> {
    await this.page.getByTestId('edit-draft-button').click();
    await this.page.locator('h2').filter({ hasText: /Edit Case/ }).waitFor({ state: 'visible', timeout: 5000 });
  }

  /**
   * Click the Discard button shown in draft mode and confirm the
   * browser-native dialog.
   */
  async clickDiscardDraft(): Promise<void> {
    this.page.once('dialog', (d) => d.accept());
    const responsePromise = this.page.waitForResponse(
      (resp) => resp.url().includes('/graphql') && resp.status() === 200,
    );
    await this.page.getByTestId('discard-draft-button').click();
    await responsePromise;
  }

  /**
   * True when the page is showing draft-mode header controls
   * (Edit/Discard) rather than Close/Reopen.
   */
  async isDraftHeader(): Promise<boolean> {
    return await this.page.getByTestId('edit-draft-button').isVisible();
  }

  /**
   * True when the aside Status pill shows Draft. The dedicated title-row
   * chip was removed; the side panel is now the single source for the
   * status indicator.
   */
  async isDraftBadgeVisible(): Promise<boolean> {
    const text = await this.page.getByTestId('aside-status-display').innerText().catch(() => '');
    return /draft/i.test(text);
  }

  /**
   * True when the Related Actions section is rendered. Drafts hide it
   * entirely (no slack channel, no actions yet).
   */
  async isRelatedActionsSectionVisible(): Promise<boolean> {
    return await this.page.locator('h3').filter({ hasText: 'No actions yet' }).first().isVisible()
      .catch(() => false);
  }

  /**
   * True when the "Open in action list" link is visible in the Related
   * Actions section header. The link is hidden for DRAFT cases because
   * the entire section is hidden in that state.
   */
  async isOpenInActionsLinkVisible(): Promise<boolean> {
    return await this.page.getByTestId('case-open-in-actions').isVisible().catch(() => false);
  }

  /**
   * Click the "Open in action list" link in the Related Actions section
   * header. Resolves once the URL has switched to the actions list page.
   */
  async clickOpenInActions(): Promise<void> {
    await this.page.getByTestId('case-open-in-actions').click();
    await this.page.waitForURL(/\/actions\/case\/\d+/, { timeout: 5000 });
  }
}
