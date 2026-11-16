import { describe, expect, it } from 'vitest';
import {
  BOARD_GROUP_BY_ASSIGNEE,
  BOARD_GROUP_BY_CUSTOMER,
  BOARD_GROUP_BY_PROJECT,
  BOARD_GROUP_BY_STATUS,
  boardLaneKeyForItem,
  buildBoardSwimlanes,
  createEffectiveProjectResolver,
  isValueGroupByDimension,
  parseBoardGroupBy,
  serializeBoardGroupBy,
} from './boardSwimlanes.js';
import { SWIMLANE_NONE_KEY } from './swimlanes.js';

const users = [
  { id: 2, full_name: 'Bo Aldrin' },
  { id: 1, full_name: 'Ada Lovelace' },
];
const statuses = [
  { id: 10, name: 'To do' },
  { id: 11, name: 'Done' },
];
const projects = [
  { id: 3, name: 'Apollo' },
  { id: 4, name: 'Gemini' },
];

function laneTitles(lanes) {
  return lanes.map((lane) => lane.title);
}

describe('group-by serialization', () => {
  it('round-trips the value dimensions', () => {
    for (const dimension of [
      BOARD_GROUP_BY_ASSIGNEE,
      BOARD_GROUP_BY_STATUS,
      BOARD_GROUP_BY_PROJECT,
    ]) {
      expect(isValueGroupByDimension(dimension)).toBe(true);
      expect(serializeBoardGroupBy({ dimension })).toBe(dimension);
      expect(parseBoardGroupBy(dimension)).toEqual({ dimension });
    }
  });

  it('keeps unknown stored values on the null grouping', () => {
    expect(parseBoardGroupBy('bogus')).toBeNull();
    expect(parseBoardGroupBy('')).toBeNull();
  });

  it('round-trips the customer container dimension without treating it as a value dimension', () => {
    expect(serializeBoardGroupBy({ dimension: BOARD_GROUP_BY_CUSTOMER })).toBe('customer');
    expect(parseBoardGroupBy('customer')).toEqual({ dimension: BOARD_GROUP_BY_CUSTOMER });
    expect(isValueGroupByDimension(BOARD_GROUP_BY_CUSTOMER)).toBe(false);
  });
});

describe('effective project resolution', () => {
  const items = [
    { id: 1, project_id: 3, inherit_project: false, project_name: 'Apollo' },
    { id: 2, parent_id: 1, inherit_project: true },
    { id: 3, parent_id: 1, project_id: null, inherit_project: false },
    { id: 4, parent_id: 99, inherit_project: true },
    { id: 5, parent_id: 5, inherit_project: true, project_id: null },
  ];

  it('walks inherit_project up the loaded parent chain', () => {
    const resolve = createEffectiveProjectResolver(items);
    expect(resolve(items[0])).toEqual({ id: 3, name: 'Apollo' });
    expect(resolve(items[1])).toEqual({ id: 3, name: 'Apollo' });
  });

  it('keeps an explicit none and degrades unloadable parents to none', () => {
    const resolve = createEffectiveProjectResolver(items);
    expect(resolve(items[2])).toBeNull();
    expect(resolve(items[3])).toBeNull();
    // Self-parent cycles must not hang or resolve.
    expect(resolve(items[4])).toBeNull();
  });

  it('builds project lane keys from the resolved project', () => {
    const resolve = createEffectiveProjectResolver(items);
    expect(
      boardLaneKeyForItem(items[1], BOARD_GROUP_BY_PROJECT, { resolveEffectiveProject: resolve })
    ).toBe('project-3');
    expect(
      boardLaneKeyForItem(items[2], BOARD_GROUP_BY_PROJECT, { resolveEffectiveProject: resolve })
    ).toBe(SWIMLANE_NONE_KEY);
  });
});

describe('value lanes', () => {
  it('groups cards by assignee with a reference-ordered unassigned lane', () => {
    const filteredItems = [
      { id: 1, assignee_id: 2 },
      { id: 2, assignee_id: 1 },
      { id: 3, assignee_id: 1 },
      { id: 4, assignee_id: null },
    ];
    const lanes = buildBoardSwimlanes({
      groupBy: { dimension: BOARD_GROUP_BY_ASSIGNEE },
      items: filteredItems,
      filteredItems,
      users,
      unassignedTitle: 'Unassigned',
    });
    expect(laneTitles(lanes)).toEqual(['Ada Lovelace', 'Bo Aldrin', 'Unassigned']);
    expect(lanes[0].itemCount).toBe(2);
    expect(lanes.at(-1).isUnassigned).toBe(true);
    expect(lanes.at(-1).itemCount).toBe(1);
  });

  it('orders status lanes by the status reference and falls back to card titles', () => {
    const filteredItems = [
      { id: 1, status_id: 11 },
      { id: 2, status_id: 99, status_name: 'Custom' },
      { id: 3, status_id: 10 },
    ];
    const lanes = buildBoardSwimlanes({
      groupBy: { dimension: BOARD_GROUP_BY_STATUS },
      items: filteredItems,
      filteredItems,
      statuses,
      unassignedTitle: 'Unassigned',
    });
    // Reference statuses come first in workflow order; the unknown status
    // sorts after them from its card title.
    expect(laneTitles(lanes)).toEqual(['To do', 'Done', 'Custom']);
    expect(lanes.every((lane) => lane.dimension === BOARD_GROUP_BY_STATUS)).toBe(true);
  });

  it('titles project lanes from the effective project of inheriting cards', () => {
    const items = [
      { id: 1, project_id: 3, inherit_project: false, project_name: 'Apollo' },
      { id: 2, parent_id: 1, inherit_project: true },
      { id: 3, parent_id: 1, project_id: null, inherit_project: false },
    ];
    const resolveEffectiveProject = createEffectiveProjectResolver(items);
    const lanes = buildBoardSwimlanes({
      groupBy: { dimension: BOARD_GROUP_BY_PROJECT },
      items,
      filteredItems: items,
      projects,
      resolveEffectiveProject,
      unassignedTitle: 'Unassigned',
    });
    expect(laneTitles(lanes)).toEqual(['Apollo', 'Unassigned']);
    expect(lanes[0].itemCount).toBe(2);
    expect(lanes.at(-1).itemCount).toBe(1);
  });

  it('returns a single all-items lane without a grouping', () => {
    const lanes = buildBoardSwimlanes({
      groupBy: null,
      items: [{ id: 1 }],
      allItemsTitle: 'All items',
    });
    expect(laneTitles(lanes)).toEqual(['All items']);
  });
});

describe('customer → project lanes', () => {
  const projects = [
    { id: 3, name: 'Apollo', customer_id: 7, customer_name: 'Acme Co' },
    { id: 4, name: 'Gemini', customer_id: 7, customer_name: 'Acme Co' },
    { id: 5, name: 'Zephyr', customer_id: 11, customer_name: 'Beta GmbH' },
    { id: 6, name: 'Orbit', customer_builtin_key: 'no_customer' },
  ];
  const items = [
    { id: 1, project_id: 3, inherit_project: false, project_name: 'Apollo' },
    { id: 2, project_id: 4, inherit_project: false, project_name: 'Gemini' },
    { id: 3, project_id: 6, inherit_project: false, project_name: 'Orbit' },
    // Inherits Apollo from its parent, so it lands with Acme Co.
    { id: 4, parent_id: 1, inherit_project: true },
    { id: 5, project_id: null, inherit_project: false },
    { id: 6, project_id: 5, inherit_project: false, project_name: 'Zephyr' },
  ];

  function customerLanes() {
    return buildBoardSwimlanes({
      groupBy: { dimension: BOARD_GROUP_BY_CUSTOMER },
      items,
      filteredItems: items,
      projects,
      resolveEffectiveProject: createEffectiveProjectResolver(items),
      noCustomerTitle: 'No customer',
      noProjectTitle: 'No project',
    });
  }

  it('nests project lanes under their customer, alphabetically with no-customer last', () => {
    const lanes = customerLanes();
    expect(laneTitles(lanes)).toEqual(['Acme Co', 'Beta GmbH', 'No customer']);

    const [acme, beta, none] = lanes;
    expect(acme.dimension).toBe(BOARD_GROUP_BY_CUSTOMER);
    expect(acme.isContainer).toBe(true);
    expect(acme.itemCount).toBe(3);
    expect(laneTitles(acme.childLanes)).toEqual(['Apollo', 'Gemini']);
    expect(acme.childLanes.every((lane) => lane.dimension === BOARD_GROUP_BY_PROJECT)).toBe(true);

    expect(beta.itemCount).toBe(1);
    expect(laneTitles(beta.childLanes)).toEqual(['Zephyr']);

    // Placeholder-customer and projectless cards collect in the container,
    // with the projectless lane sorted last.
    expect(none.itemCount).toBe(2);
    expect(laneTitles(none.childLanes)).toEqual(['Orbit', 'No project']);
    expect(none.childLanes.at(-1).key).toBe(SWIMLANE_NONE_KEY);
  });
});
