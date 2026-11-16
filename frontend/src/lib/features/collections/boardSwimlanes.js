// Board-specific swimlane model. Kept pure so lane grouping, ordering and the
// group-by preference can be unit-tested without mounting the board.
//
// The board supports three modes:
// - none: one lane holding every card
// - item-type: lanes are items of the selected type; their direct children are
//   the cards, everything else is unassigned
// - milestone: one lane per milestone using the item's primary milestone, plus
//   a trailing unassigned lane

import { NO_CUSTOMER_BUILTIN_KEY } from '../../utils/customerLabel.js';
import {
  laneKeyForItem,
  primaryLaneKeyForItem,
  SWIMLANE_NONE_KEY,
  sortMilestonesForLanes,
} from './swimlanes.js';

export const BOARD_GROUP_BY_NONE = 'none';
export const BOARD_GROUP_BY_ITEM_TYPE = 'item-type';
export const BOARD_GROUP_BY_MILESTONE = 'milestone';
export const BOARD_GROUP_BY_ASSIGNEE = 'assignee';
export const BOARD_GROUP_BY_STATUS = 'status';
export const BOARD_GROUP_BY_PROJECT = 'project';
// Container dimension: a customer lane holding project sub-lanes.
export const BOARD_GROUP_BY_CUSTOMER = 'customer';

// Dimensions serialized as their plain name (no auxiliary id).
const PLAIN_GROUP_BY_DIMENSIONS = new Set([
  BOARD_GROUP_BY_MILESTONE,
  BOARD_GROUP_BY_ASSIGNEE,
  BOARD_GROUP_BY_STATUS,
  BOARD_GROUP_BY_PROJECT,
  BOARD_GROUP_BY_CUSTOMER,
]);

// Dimensions whose lanes are value buckets (one lane per distinct value plus
// an unassigned lane) instead of entity parents.
export function isValueGroupByDimension(dimension) {
  return (
    dimension === BOARD_GROUP_BY_ASSIGNEE ||
    dimension === BOARD_GROUP_BY_STATUS ||
    dimension === BOARD_GROUP_BY_PROJECT
  );
}

// Lane key for cards whose completed milestone lane is hidden by the board
// configuration. The lane keeps those cards visible instead of dropping them.
export const MILESTONE_HIDDEN_COMPLETED_KEY = 'milestone-completed-hidden';

// Lane key for open cards whose primary milestone is completed or cancelled.
// Surfaces work that is likely assigned to the wrong milestone so it can be
// moved to the right one.
export const MILESTONE_OPEN_FROM_COMPLETED_KEY = 'milestone-open-from-completed';

const INACTIVE_MILESTONE_STATUSES = new Set(['completed', 'cancelled']);

// Default for the milestone rescue lane when the caller does not supply a
// status-aware predicate: every card counts as open.
/** @type {(item: object) => boolean} */
const defaultIsItemOpen = () => true;

export function boardGroupPreferenceStorageKey(scope) {
  return `board-group-by-${scope}`;
}

export function legacyBoardGroupItemTypeStorageKey(scope) {
  return `board-group-by-item-type-${scope}`;
}

export function serializeBoardGroupBy(groupBy) {
  if (!groupBy) return null;
  if (PLAIN_GROUP_BY_DIMENSIONS.has(groupBy.dimension)) return groupBy.dimension;
  if (groupBy.dimension === BOARD_GROUP_BY_ITEM_TYPE && groupBy.itemTypeId != null) {
    return `type:${groupBy.itemTypeId}`;
  }
  return null;
}

export function parseBoardGroupBy(value) {
  const raw = typeof value === 'string' ? value.trim() : '';
  if (!raw) return null;
  if (PLAIN_GROUP_BY_DIMENSIONS.has(raw)) return { dimension: raw };
  const typeMatch = /^type:(\d+)$/.exec(raw);
  if (typeMatch) {
    return { dimension: BOARD_GROUP_BY_ITEM_TYPE, itemTypeId: Number(typeMatch[1]) };
  }
  return null;
}

// The item-type preference used a separate numeric storage key before the
// group-by menu gained milestone lanes. Read the new key first and fall back
// to the legacy one for users upgrading with a saved preference.
export function resolveStoredBoardGroupBy(currentValue, legacyItemTypeValue, itemTypes = []) {
  const current = parseBoardGroupBy(currentValue);
  if (current) {
    if (
      current.dimension === BOARD_GROUP_BY_ITEM_TYPE &&
      itemTypes.length > 0 &&
      !itemTypes.some((type) => type.id === current.itemTypeId)
    ) {
      return null;
    }
    return current;
  }

  const legacyId = parseInt(legacyItemTypeValue, 10);
  if (!Number.isFinite(legacyId)) return null;
  if (itemTypes.length > 0 && !itemTypes.some((type) => type.id === legacyId)) return null;
  return { dimension: BOARD_GROUP_BY_ITEM_TYPE, itemTypeId: legacyId };
}

export function isMilestoneSwimlaneActive(milestone) {
  return !INACTIVE_MILESTONE_STATUSES.has(String(milestone?.status || '').toLowerCase());
}

// Deduplicated milestone reference derived from the loaded cards. Used on
// cross-workspace boards where no global milestone catalog is loaded (WI-1587).
export function milestonesFromItems(items = []) {
  const byId = new Map();
  for (const item of items) {
    for (const milestone of item?.milestones || []) {
      if (milestone?.id == null || byId.has(milestone.id)) continue;
      byId.set(milestone.id, milestone);
    }
  }
  return sortMilestonesForLanes([...byId.values()]);
}

// Effective (inheritance-resolved) direct project for one item, mirroring the
// backend: a direct project wins, inherit_project walks up the loaded parent
// chain, an explicit no-project stays none, and an unloadable parent degrades
// to none instead of hiding the card. Returns {id, name} | null.
/**
 * @param {Array<object>} [items]
 * @returns {(item: object) => {id: number, name: string|null}|null}
 */
export function createEffectiveProjectResolver(items = []) {
  const byId = new Map();
  for (const item of items) byId.set(item.id, item);

  const memo = new Map();
  const visiting = new Set();

  function resolve(item) {
    if (!item) return null;
    if (memo.has(item.id)) return memo.get(item.id);
    if (visiting.has(item.id)) return null;
    visiting.add(item.id);

    let result = null;
    if (item.project_id != null && !item.inherit_project) {
      result = { id: item.project_id, name: item.project_name || null };
    } else if (!item.inherit_project) {
      result = null;
    } else if (item.parent_id != null && item.parent_id !== item.id) {
      result = resolve(byId.get(item.parent_id));
    }

    visiting.delete(item.id);
    memo.set(item.id, result);
    return result;
  }

  return (item) => resolve(item);
}

// Lane key for one card under the board's current grouping. Value dimensions
// share swimlanes.js's keys; project grouping resolves inheritance first.
/**
 * @param {object} item
 * @param {string|null} dimension
 * @param {{milestones?: Array, resolveEffectiveProject?: (item: object) => {id: number, name: string|null}|null}} [options]
 * @returns {string}
 */
export function boardLaneKeyForItem(
  item,
  dimension,
  { milestones = [], resolveEffectiveProject } = {}
) {
  if (dimension === BOARD_GROUP_BY_MILESTONE) {
    return primaryLaneKeyForItem(item, dimension, milestones);
  }
  if (dimension === BOARD_GROUP_BY_PROJECT) {
    const project = resolveEffectiveProject ? resolveEffectiveProject(item) : null;
    return project ? `project-${project.id}` : SWIMLANE_NONE_KEY;
  }
  return laneKeyForItem(item, dimension);
}

// Customer for a card, derived through its effective project. Projects
// without a customer — and the built-in "no customer" placeholder org —
// resolve to null so those cards collect in the no-customer container.
/**
 * @param {Array<object>} [projects]
 * @param {(item: object) => {id: number, name: string|null}|null} [resolveEffectiveProject]
 * @returns {(item: object) => {id: number, name: string|null}|null}
 */
export function createCustomerResolver(projects = [], resolveEffectiveProject = () => null) {
  const projectById = new Map();
  for (const project of projects || []) {
    if (project?.id != null) projectById.set(Number(project.id), project);
  }
  return (item) => {
    const project = resolveEffectiveProject(item);
    if (!project) return null;
    const reference = projectById.get(Number(project.id));
    if (!reference) return null; // project unknown on this board → no customer
    if (reference.customer_builtin_key === NO_CUSTOMER_BUILTIN_KEY) return null;
    const customerId = reference.customer_id ?? null;
    if (customerId == null) return null;
    return { id: customerId, name: reference.customer_name || null };
  };
}

// Container lane key for a card's customer, or the shared no-customer key.
export function customerLaneKeyForItem(item, resolveCustomer) {
  const customer = resolveCustomer ? resolveCustomer(item) : null;
  return customer ? `customer-${customer.id}` : 'customer-none';
}

// Customer → project lanes: one container lane per customer, each holding a
// project sub-lane per distinct effective project. Containers are display
// bands only — cards and drop targets live on the project sub-lanes, whose
// keys match the standalone project grouping so drops reuse its update path.
/**
 * @param {object} options
 * @param {Array} options.filteredItems
 * @param {(item: object) => {id: number, name: string|null}|null} [options.resolveEffectiveProject]
 * @param {(item: object) => {id: number, name: string|null}|null} [options.resolveCustomer]
 * @param {string} options.noCustomerTitle
 * @param {string} options.noProjectTitle
 */
function customerProjectLanes({
  filteredItems,
  resolveEffectiveProject = () => null,
  resolveCustomer = () => null,
  noCustomerTitle,
  noProjectTitle,
}) {
  /** @type {Map<string, {key: string, title: string|null, items: object[], projects: Map<string, {key: string, title: string|null, items: object[]}>}>} */
  const containers = new Map();
  const containerFor = (customer) => {
    const key = customer ? `customer-${customer.id}` : 'customer-none';
    let container = containers.get(key);
    if (!container) {
      container = {
        key,
        title: customer ? customer.name : noCustomerTitle,
        items: [],
        projects: new Map(),
      };
      containers.set(key, container);
    }
    return container;
  };

  for (const item of filteredItems) {
    const container = containerFor(resolveCustomer(item));
    const project = resolveEffectiveProject(item);
    const projectKey = project ? `project-${project.id}` : SWIMLANE_NONE_KEY;
    let lane = container.projects.get(projectKey);
    if (!lane) {
      lane = {
        key: projectKey,
        title: project
          ? project.name
          : item.project_name || (projectKey === SWIMLANE_NONE_KEY ? noProjectTitle : null),
        items: [],
      };
      container.projects.set(projectKey, lane);
    } else if (!lane.title && item.project_name) {
      lane.title = item.project_name;
    }
    lane.items.push(item);
    container.items.push(item);
  }

  const orderedContainers = [...containers.values()].sort((a, b) => {
    if (a.key === 'customer-none') return 1;
    if (b.key === 'customer-none') return -1;
    return String(a.title || '').localeCompare(String(b.title || ''));
  });

  return orderedContainers.map((container) => {
    const projectLanes = [...container.projects.values()]
      .map((lane) => ({
        id: lane.key,
        key: lane.key,
        dimension: BOARD_GROUP_BY_PROJECT,
        title: lane.title || `Project #${String(lane.key).split('-').pop()}`,
        parent: null,
        milestone: null,
        items: lane.items,
        isDefault: false,
        isUnassigned: lane.key === SWIMLANE_NONE_KEY,
        itemCount: lane.items.length,
        parentIsVisible: false,
      }))
      .sort((a, b) => {
        if (a.isUnassigned) return 1;
        if (b.isUnassigned) return -1;
        const byTitle = String(a.title).localeCompare(String(b.title));
        if (byTitle !== 0) return byTitle;
        return String(a.key).localeCompare(String(b.key));
      });

    return {
      id: container.key,
      key: container.key,
      dimension: BOARD_GROUP_BY_CUSTOMER,
      title: container.title || noCustomerTitle,
      parent: null,
      milestone: null,
      items: container.items,
      isDefault: false,
      isUnassigned: false,
      isContainer: true,
      itemCount: container.items.length,
      parentIsVisible: false,
      childLanes: projectLanes,
    };
  });
}

function unassignedLane(title, items, dimension) {
  return {
    // DOM/test id stays "unassigned"; the shared lane key is the neutral
    // SWIMLANE_NONE_KEY used by membership helpers.
    id: 'unassigned',
    key: SWIMLANE_NONE_KEY,
    dimension,
    title,
    parent: null,
    milestone: null,
    items,
    isDefault: false,
    isUnassigned: true,
    itemCount: items.length,
    parentIsVisible: false,
  };
}

function itemTypeLanes({
  items,
  filteredItems,
  itemTypeId,
  excludeRightmostSwimlaneParents,
  rightmostStatusIds,
  statusIdForItem,
  unassignedTitle,
}) {
  const isExcludedRightmostParent = (item) =>
    Boolean(excludeRightmostSwimlaneParents && rightmostStatusIds.has(statusIdForItem(item)));
  const isEligibleSwimlaneParent = (item) =>
    item.item_type_id === itemTypeId && !isExcludedRightmostParent(item);

  const parentItems = items.filter(isEligibleSwimlaneParent);
  const parentIds = new Set(parentItems.map((item) => item.id));
  const visibleItemIds = new Set(filteredItems.map((item) => item.id));

  // Bucket children by parent once instead of rescanning filteredItems per
  // parent; the scan is O(parents × items) on large boards.
  const childrenByParent = new Map();
  for (const item of filteredItems) {
    if (item.parent_id == null || item.parent_id === item.id) continue;
    const bucket = childrenByParent.get(item.parent_id);
    if (bucket) bucket.push(item);
    else childrenByParent.set(item.parent_id, [item]);
  }

  const lanes = parentItems
    .map((parent) => {
      const laneItems = childrenByParent.get(parent.id) ?? [];
      return {
        id: `parent-${parent.id}`,
        key: `parent-${parent.id}`,
        dimension: BOARD_GROUP_BY_ITEM_TYPE,
        title: parent.title,
        parent,
        milestone: null,
        items: laneItems,
        isDefault: false,
        isUnassigned: false,
        itemCount: laneItems.length,
        parentIsVisible: visibleItemIds.has(parent.id),
      };
    })
    .filter((lane) => lane.itemCount > 0 || lane.parentIsVisible);

  const unassignedItems = filteredItems.filter((item) => {
    if (item.item_type_id === itemTypeId) return false;
    if (item.parent_id && parentIds.has(item.parent_id)) return false;
    return true;
  });

  return [...lanes, unassignedLane(unassignedTitle, unassignedItems, BOARD_GROUP_BY_ITEM_TYPE)];
}

function hiddenCompletedLane(items, title) {
  return {
    id: MILESTONE_HIDDEN_COMPLETED_KEY,
    key: MILESTONE_HIDDEN_COMPLETED_KEY,
    dimension: BOARD_GROUP_BY_MILESTONE,
    title,
    parent: null,
    milestone: null,
    items,
    isDefault: false,
    isUnassigned: false,
    isAggregate: true,
    itemCount: items.length,
    parentIsVisible: false,
  };
}

function openFromCompletedLane(items, title) {
  return {
    id: MILESTONE_OPEN_FROM_COMPLETED_KEY,
    key: MILESTONE_OPEN_FROM_COMPLETED_KEY,
    dimension: BOARD_GROUP_BY_MILESTONE,
    title,
    parent: null,
    milestone: null,
    items,
    isDefault: false,
    isUnassigned: false,
    isAggregate: true,
    isRescue: true,
    itemCount: items.length,
    parentIsVisible: false,
  };
}

function milestoneLanes({
  filteredItems,
  milestones,
  includeEmptyMilestones,
  globalMilestoneIds = [],
  showCompletedMilestones = true,
  showOpenFromCompletedMilestones = false,
  isItemOpen = defaultIsItemOpen,
  unassignedTitle,
  hiddenCompletedTitle,
  openFromCompletedTitle,
}) {
  const orderedMilestones = sortMilestonesForLanes(milestones);
  const referenceById = new Map(
    orderedMilestones.map((milestone) => [Number(milestone.id), milestone])
  );
  const pinnedGlobalIds = new Set((globalMilestoneIds || []).map(Number));

  // Bucket by primary lane once; every lane filter below is a Map lookup.
  const bucket = new Map();
  for (const item of filteredItems) {
    const key = primaryLaneKeyForItem(item, BOARD_GROUP_BY_MILESTONE, orderedMilestones);
    const laneItems = bucket.get(key);
    if (laneItems) laneItems.push(item);
    else bucket.set(key, [item]);
  }

  const embeddedById = new Map();
  for (const item of filteredItems) {
    for (const milestone of item?.milestones || []) {
      if (milestone?.id != null && !embeddedById.has(milestone.id)) {
        embeddedById.set(milestone.id, milestone);
      }
    }
  }

  // Open cards on a completed or cancelled milestone are likely mis-assigned.
  // Pull them into a dedicated rescue lane so they can be moved to the right
  // milestone instead of hiding inside a finished lane or the aggregate.
  const rescueItems = [];
  if (showOpenFromCompletedMilestones) {
    for (const [key, laneItems] of [...bucket]) {
      if (key === SWIMLANE_NONE_KEY) continue;
      const id = Number(String(key).split('-').pop());
      if (!Number.isFinite(id)) continue;
      const milestone = referenceById.get(id) || embeddedById.get(id);
      if (!milestone || isMilestoneSwimlaneActive(milestone)) continue;
      const remaining = [];
      for (const item of laneItems) {
        if (isItemOpen(item)) rescueItems.push(item);
        else remaining.push(item);
      }
      if (remaining.length > 0) bucket.set(key, remaining);
      else bucket.delete(key);
    }
  }

  const lanes = [];
  const emittedIds = new Set();
  // Cards whose completed lane is hidden still have to render somewhere; they
  // collect into one aggregate lane instead of disappearing from the board.
  const hiddenCompletedItems = [];

  for (const milestone of orderedMilestones) {
    const key = `milestone-${milestone.id}`;
    const laneItems = bucket.get(key) ?? [];
    const active = isMilestoneSwimlaneActive(milestone);
    if (!active && !showCompletedMilestones) {
      hiddenCompletedItems.push(...laneItems);
      emittedIds.add(milestone.id);
      continue;
    }
    if (laneItems.length === 0) {
      if (!includeEmptyMilestones || !active) continue;
      // Global milestones are shared across every workspace, so an empty lane
      // is only offered when the board explicitly pinned that milestone.
      if (milestone.is_global && !pinnedGlobalIds.has(Number(milestone.id))) continue;
    }
    emittedIds.add(milestone.id);
    lanes.push({
      id: key,
      key,
      dimension: BOARD_GROUP_BY_MILESTONE,
      title: milestone.name,
      parent: null,
      milestone,
      items: laneItems,
      isDefault: false,
      isUnassigned: false,
      itemCount: laneItems.length,
      parentIsVisible: false,
    });
  }

  // Guard against a stale catalog: cards attached to a milestone missing from
  // the reference still get a lane instead of silently dropping into unassigned.
  const fallback = [];
  for (const [key, laneItems] of bucket) {
    if (key === SWIMLANE_NONE_KEY || laneItems.length === 0) continue;
    const id = Number(String(key).split('-').pop());
    if (!Number.isFinite(id) || emittedIds.has(id)) continue;
    const milestone = referenceById.get(id) ||
      embeddedById.get(id) || { id, name: `Milestone #${id}` };
    if (!isMilestoneSwimlaneActive(milestone) && !showCompletedMilestones) {
      hiddenCompletedItems.push(...laneItems);
      emittedIds.add(id);
      continue;
    }
    emittedIds.add(id);
    fallback.push({
      id: key,
      key,
      dimension: BOARD_GROUP_BY_MILESTONE,
      title: milestone.name || `Milestone #${id}`,
      parent: null,
      milestone,
      items: laneItems,
      isDefault: false,
      isUnassigned: false,
      itemCount: laneItems.length,
      parentIsVisible: false,
    });
  }
  fallback.sort((a, b) => {
    const byDate = compareLaneDates(a.milestone, b.milestone);
    if (byDate !== 0) return byDate;
    return String(a.title).localeCompare(String(b.title));
  });

  const unassignedItems = bucket.get(SWIMLANE_NONE_KEY) ?? [];
  const result = [];
  if (rescueItems.length > 0) {
    result.push(openFromCompletedLane(rescueItems, openFromCompletedTitle));
  }
  result.push(...lanes, ...fallback);
  if (hiddenCompletedItems.length > 0) {
    result.push(hiddenCompletedLane(hiddenCompletedItems, hiddenCompletedTitle));
  }
  result.push(unassignedLane(unassignedTitle, unassignedItems, BOARD_GROUP_BY_MILESTONE));
  return result;
}

// Lanes for the value dimensions (assignee, status, project): one lane per
// distinct value plus a trailing unassigned lane. Reference lists order the
// lanes; cards whose value is missing from the reference (cross-workspace
// boards) still get a lane titled from the card itself.
/**
 * @param {object} options
 * @param {string} options.dimension
 * @param {Array} options.filteredItems
 * @param {Array} [options.users]
 * @param {Array} [options.statuses]
 * @param {Array} [options.projects]
 * @param {(item: object) => {id: number, name: string|null}|null} [options.resolveEffectiveProject]
 * @param {string} options.unassignedTitle
 */
function valueLanes({
  dimension,
  filteredItems,
  users = [],
  statuses = [],
  projects = [],
  resolveEffectiveProject = () => null,
  unassignedTitle,
}) {
  const bucket = new Map();
  for (const item of filteredItems) {
    const key = boardLaneKeyForItem(item, dimension, { resolveEffectiveProject });
    const laneItems = bucket.get(key);
    if (laneItems) laneItems.push(item);
    else bucket.set(key, [item]);
  }

  const laneObjects = [];
  const emitted = new Set();
  const pushLane = (key, title, laneItems) => {
    emitted.add(key);
    laneObjects.push({
      // The unassigned lane keeps the shared "unassigned" DOM/test id used by
      // the milestone and item-type groupings.
      id: key === SWIMLANE_NONE_KEY ? 'unassigned' : key,
      key,
      dimension,
      title,
      parent: null,
      milestone: null,
      items: laneItems,
      isDefault: false,
      isUnassigned: key === SWIMLANE_NONE_KEY,
      itemCount: laneItems.length,
      parentIsVisible: false,
    });
  };

  // Reference-driven order: statuses in workflow order, users and projects
  // alphabetically.
  const reference = [];
  if (dimension === BOARD_GROUP_BY_STATUS) {
    for (const status of statuses) {
      reference.push({
        key: `status-${status.id}`,
        title: status.display_name || status.name || `Status #${status.id}`,
      });
    }
  } else if (dimension === BOARD_GROUP_BY_ASSIGNEE) {
    const sorted = [...users].sort((a, b) =>
      String(a.full_name || a.name || '').localeCompare(String(b.full_name || b.name || ''))
    );
    for (const user of sorted) {
      reference.push({
        key: `assignee-${user.id}`,
        title: user.full_name || user.name || `User #${user.id}`,
      });
    }
  } else if (dimension === BOARD_GROUP_BY_PROJECT) {
    const sorted = [...projects].sort((a, b) =>
      String(a.name || '').localeCompare(String(b.name || ''))
    );
    for (const project of sorted) {
      reference.push({
        key: `project-${project.id}`,
        title: project.name || `Project #${project.id}`,
      });
    }
  }
  for (const entry of reference) {
    const laneItems = bucket.get(entry.key);
    if (!laneItems || laneItems.length === 0) continue;
    pushLane(entry.key, entry.title, laneItems);
  }

  // Lanes not covered by the reference, titled from the cards themselves.
  const fallbackLanes = [];
  for (const [key, laneItems] of bucket) {
    if (key === SWIMLANE_NONE_KEY || emitted.has(key) || laneItems.length === 0) continue;
    const first = laneItems.find(Boolean);
    let title;
    if (dimension === BOARD_GROUP_BY_ASSIGNEE) {
      title = first?.assignee_name || `User #${key.split('-').pop()}`;
    } else if (dimension === BOARD_GROUP_BY_STATUS) {
      title = first?.status_name || `Status #${key.split('-').pop()}`;
    } else {
      title =
        resolveEffectiveProject(first)?.name ||
        first?.project_name ||
        `Project #${key.split('-').pop()}`;
    }
    fallbackLanes.push({ key, title, items: laneItems });
  }
  fallbackLanes.sort((a, b) => {
    const byTitle = String(a.title).localeCompare(String(b.title));
    if (byTitle !== 0) return byTitle;
    return a.key.localeCompare(b.key);
  });
  for (const entry of fallbackLanes) pushLane(entry.key, entry.title, entry.items);

  const unassignedItems = bucket.get(SWIMLANE_NONE_KEY) ?? [];
  if (unassignedItems.length > 0) {
    pushLane(SWIMLANE_NONE_KEY, unassignedTitle, unassignedItems);
  }
  return laneObjects;
}

function compareLaneDates(a, b) {
  const aDate = a?.target_date == null || a.target_date === '' ? null : String(a.target_date);
  const bDate = b?.target_date == null || b.target_date === '' ? null : String(b.target_date);
  if (aDate === bDate) return 0;
  if (aDate === null) return 1;
  if (bDate === null) return -1;
  return aDate.localeCompare(bDate);
}

/**
 * Build the board's swimlanes for the current grouping mode.
 *
 * @param {object} options
 * @param {{dimension: string, itemTypeId?: number}|null} options.groupBy
 * @param {Array} [options.items] all loaded cards (used for item-type parent eligibility)
 * @param {Array} [options.filteredItems] cards after client filters
 * @param {Array} [options.milestones] milestone reference for lane headers/order
 * @param {Array} [options.users] user reference for assignee lanes
 * @param {Array} [options.statuses] status reference for status lanes
 * @param {Array} [options.projects] time-project reference for project lanes
 * @param {(item: object) => {id: number, name: string|null}|null} [options.resolveEffectiveProject]
 * @param {(item: object) => number|null} [options.statusIdForItem]
 * @param {Set<number>} [options.rightmostStatusIds]
 * @param {boolean} [options.excludeRightmostSwimlaneParents]
 * @param {boolean} [options.includeEmptyMilestones]
 * @param {number[]} [options.globalMilestoneIds] pinned global milestones that keep an empty lane
 * @param {boolean} [options.showCompletedMilestones] render completed milestone lanes
 * @param {boolean} [options.showOpenFromCompletedMilestones] collect open items on completed milestones into a rescue lane
 * @param {(item: object) => boolean} [options.isItemOpen] whether a card is still open
 * @param {string} [options.unassignedTitle]
 * @param {string} [options.noCustomerTitle] title of the no-customer container on customer lanes
 * @param {string} [options.noProjectTitle] title of the projectless sub-lane on customer lanes
 * @param {string} [options.hiddenCompletedTitle] title of the aggregate lane for hidden completed milestones
 * @param {string} [options.openFromCompletedTitle] title of the rescue lane for open items on completed milestones
 * @param {string} [options.allItemsTitle]
 */
export function buildBoardSwimlanes({
  groupBy,
  items = [],
  filteredItems = items,
  milestones = [],
  users = [],
  statuses = [],
  projects = [],
  resolveEffectiveProject = () => null,
  statusIdForItem = (item) => item.status_id ?? null,
  rightmostStatusIds = new Set(),
  excludeRightmostSwimlaneParents = false,
  includeEmptyMilestones = true,
  globalMilestoneIds = [],
  showCompletedMilestones = true,
  showOpenFromCompletedMilestones = false,
  isItemOpen = defaultIsItemOpen,
  unassignedTitle = 'Unassigned',
  noCustomerTitle = 'No customer',
  noProjectTitle = 'No project',
  hiddenCompletedTitle = 'Completed milestones',
  openFromCompletedTitle = 'Open from completed milestones',
  allItemsTitle = 'All items',
}) {
  if (!groupBy) {
    return [
      {
        id: 'all',
        key: null,
        dimension: null,
        title: allItemsTitle,
        parent: null,
        milestone: null,
        items: filteredItems,
        isDefault: true,
        isUnassigned: false,
        itemCount: filteredItems.length,
        parentIsVisible: false,
      },
    ];
  }

  if (groupBy.dimension === BOARD_GROUP_BY_MILESTONE) {
    return milestoneLanes({
      filteredItems,
      milestones,
      includeEmptyMilestones,
      globalMilestoneIds,
      showCompletedMilestones,
      showOpenFromCompletedMilestones,
      isItemOpen,
      unassignedTitle,
      hiddenCompletedTitle,
      openFromCompletedTitle,
    });
  }

  if (groupBy.dimension === BOARD_GROUP_BY_ITEM_TYPE && groupBy.itemTypeId != null) {
    return itemTypeLanes({
      items,
      filteredItems,
      itemTypeId: groupBy.itemTypeId,
      excludeRightmostSwimlaneParents,
      rightmostStatusIds,
      statusIdForItem,
      unassignedTitle,
    });
  }

  if (isValueGroupByDimension(groupBy.dimension)) {
    return valueLanes({
      dimension: groupBy.dimension,
      filteredItems,
      users,
      statuses,
      projects,
      resolveEffectiveProject,
      unassignedTitle,
    });
  }

  if (groupBy.dimension === BOARD_GROUP_BY_CUSTOMER) {
    return customerProjectLanes({
      filteredItems,
      resolveEffectiveProject,
      resolveCustomer: createCustomerResolver(projects, resolveEffectiveProject),
      noCustomerTitle,
      noProjectTitle,
    });
  }

  return buildBoardSwimlanes({
    groupBy: null,
    items,
    filteredItems,
    milestones,
    allItemsTitle,
  });
}
