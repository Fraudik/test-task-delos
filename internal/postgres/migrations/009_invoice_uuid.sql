-- +goose Up
ALTER TABLE invoices ADD COLUMN new_id UUID NOT NULL DEFAULT gen_random_uuid();

ALTER TABLE deposits ADD COLUMN new_invoice_id UUID;
UPDATE deposits d SET new_invoice_id = i.new_id FROM invoices i WHERE i.id = d.invoice_id;
ALTER TABLE deposits DROP COLUMN invoice_id;
ALTER TABLE deposits RENAME COLUMN new_invoice_id TO invoice_id;
ALTER TABLE deposits ALTER COLUMN invoice_id SET NOT NULL;

ALTER TABLE invoices DROP COLUMN id;
ALTER TABLE invoices RENAME COLUMN new_id TO id;
ALTER TABLE invoices ADD PRIMARY KEY (id);

ALTER TABLE deposits ADD CONSTRAINT deposits_invoice_id_fkey FOREIGN KEY (invoice_id) REFERENCES invoices (id);
CREATE INDEX deposits_invoice_idx ON deposits (invoice_id);
CREATE INDEX deposits_confirmed_invoice_idx ON deposits (invoice_id) WHERE status = 'confirmed';

-- +goose Down
ALTER TABLE invoices ADD COLUMN old_id BIGINT;
UPDATE invoices i SET old_id = n.rn
FROM (SELECT id, row_number() OVER (ORDER BY created_at, id) AS rn FROM invoices) n
WHERE n.id = i.id;

ALTER TABLE deposits ADD COLUMN old_invoice_id BIGINT;
UPDATE deposits d SET old_invoice_id = i.old_id FROM invoices i WHERE i.id = d.invoice_id;
ALTER TABLE deposits DROP COLUMN invoice_id;
ALTER TABLE deposits RENAME COLUMN old_invoice_id TO invoice_id;
ALTER TABLE deposits ALTER COLUMN invoice_id SET NOT NULL;

ALTER TABLE invoices DROP COLUMN id;
ALTER TABLE invoices RENAME COLUMN old_id TO id;
ALTER TABLE invoices ALTER COLUMN id SET NOT NULL;
ALTER TABLE invoices ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY;
SELECT setval(pg_get_serial_sequence('invoices', 'id'), COALESCE((SELECT max(id) FROM invoices), 0) + 1, false);
ALTER TABLE invoices ADD PRIMARY KEY (id);

ALTER TABLE deposits ADD CONSTRAINT deposits_invoice_id_fkey FOREIGN KEY (invoice_id) REFERENCES invoices (id);
CREATE INDEX deposits_invoice_idx ON deposits (invoice_id);
CREATE INDEX deposits_confirmed_invoice_idx ON deposits (invoice_id) WHERE status = 'confirmed';
