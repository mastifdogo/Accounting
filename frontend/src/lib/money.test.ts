import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseAmount, formatCents, centsToInput, MAX_CENTS } from './money.ts';

test('parseAmount accepts valid amounts', () => {
	const cases: [string, number][] = [
		['', 0],
		['0', 0],
		['1', 100],
		['1.5', 150],
		['1.05', 105],
		[' 1234.56 ', 123456],
		['1,234.56', 123456],
		['1,000,000', 100000000],
		['90071992547409.91', MAX_CENTS]
	];
	for (const [input, want] of cases) assert.equal(parseAmount(input), want, input);
});

test('parseAmount rejects invalid amounts', () => {
	for (const input of ['1.234', '-1', '+1', '$5', '1e3', '.5', '5.', 'abc', '1,23', '12,34.00', '90071992547409.92', '0.1.2']) {
		assert.equal(parseAmount(input), null, input);
	}
});

test('formatCents is exact', () => {
	assert.equal(formatCents(0), '0.00');
	assert.equal(formatCents(5), '0.05');
	assert.equal(formatCents(-123456), '-1,234.56');
	assert.equal(formatCents(100000000), '1,000,000.00');
	assert.equal(formatCents(MAX_CENTS), '90,071,992,547,409.91');
	assert.equal(centsToInput(123456), '1234.56');
	assert.equal(centsToInput(0), '');
});
