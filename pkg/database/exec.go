package database

import (
    "context"
    "database/sql"
)

func (b *Batcher) commitBatch(batch []*batchReq) {
    ctx := context.Background()
    tx, err := b.db.BeginTx(ctx, nil)
    if err != nil {
        for _, r := range batch {
            r.reply <- batchReply{err: err}
        }
        return
    }
    replies := make([]batchReply, len(batch))
    for i, r := range batch {
        replies[i] = b.execOne(ctx, tx, r)
    }
    if err := tx.Commit(); err != nil {
        for i := range replies {
            if replies[i].err == nil {
                replies[i] = batchReply{err: err}
            }
        }
    } else if b.gen != nil {
        b.gen.Add(1)
    }
    for i, r := range batch {
        r.reply <- replies[i]
    }
}

func (b *Batcher) execOne(ctx context.Context, tx *sql.Tx, r *batchReq) batchReply {
    if r.returns {
        rows, err := tx.QueryContext(ctx, r.sqlText, r.args...)
        if err != nil {
            return batchReply{err: err}
        }
        defer rows.Close()
        data, err := ScanRows(rows)
        if err != nil {
            return batchReply{err: err}
        }
        return batchReply{res: BatchResult{Rows: data, HasRows: true}}
    }
    res, err := tx.ExecContext(ctx, r.sqlText, r.args...)
    if err != nil {
        return batchReply{err: err}
    }
    n, _ := res.RowsAffected()
    return batchReply{res: BatchResult{Affected: n}}
}
