# The SQL grammar

SentinelDB's query language is defined by the context-free grammar below. `internal/sql/parser.go` implements it by recursive descent, with one method per production, and builds an explicit parse tree. Anything this grammar does not derive is rejected before any data is touched.

Run `EXPLAIN <statement>` in the shell to see the parse tree and relational algebra for any statement.

## Notation

`{ x }` means zero or more repetitions and `[ x ]` means optional. Keywords are in capitals, and terminals in quotes. Keywords are case-insensitive, and identifiers are folded to lower case.

## Statements

```
request        -> statement [ ";" ]
statement      -> create_stmt | txn_stmt | explain_stmt | dml
txn_stmt       -> BEGIN | COMMIT | ROLLBACK
explain_stmt   -> EXPLAIN dml
dml            -> select_stmt | insert_stmt | update_stmt | delete_stmt

create_stmt    -> CREATE TABLE IDENT "(" table_element { "," table_element } ")"
table_element  -> column_def | pk_constraint | fk_constraint
column_def     -> IDENT type { column_constraint }
column_constraint -> NOT NULL | PRIMARY KEY | REFERENCES IDENT "(" IDENT ")"
type           -> INT | INTEGER | TEXT | BOOL | BOOLEAN
pk_constraint  -> PRIMARY KEY "(" ident_list ")"
fk_constraint  -> FOREIGN KEY "(" IDENT ")" REFERENCES IDENT "(" IDENT ")"
ident_list     -> IDENT { "," IDENT }

insert_stmt    -> INSERT INTO IDENT [ "(" ident_list ")" ] VALUES row { "," row }
row            -> "(" expr { "," expr } ")"

select_stmt    -> SELECT select_list FROM table_ref { join_clause }
                  [ where_clause ] [ order_clause ] [ limit_clause ]
select_list    -> "*" | column_ref { "," column_ref }
table_ref      -> IDENT [ [ AS ] IDENT ]
join_clause    -> [ INNER ] JOIN table_ref ON expr
where_clause   -> WHERE expr
order_clause   -> ORDER BY column_ref [ ASC | DESC ]
limit_clause   -> LIMIT INTEGER

update_stmt    -> UPDATE IDENT SET assignment { "," assignment } [ where_clause ]
assignment     -> IDENT "=" additive
delete_stmt    -> DELETE FROM IDENT [ where_clause ]
```

## Expressions

```
expr           -> and_expr { OR and_expr }
and_expr       -> not_expr { AND not_expr }
not_expr       -> NOT not_expr | comparison
comparison     -> additive [ comp_op additive | IS [ NOT ] NULL ]
comp_op        -> "=" | "<>" | "<" | "<=" | ">" | ">="
additive       -> multiplicative { ( "+" | "-" ) multiplicative }
multiplicative -> unary { ( "*" | "/" ) unary }
unary          -> "-" unary | primary
primary        -> INTEGER | STRING | TRUE | FALSE | NULL | "?" | column_ref | "(" expr ")"
column_ref     -> IDENT [ "." IDENT ]
```

## How the grammar was made unambiguous

The natural way to write expressions is one short rule:

```
expr -> expr OR expr | expr AND expr | expr "<" expr | expr "+" expr | ... | primary
```

It has two problems, each fixed with a standard technique.

### Problem 1: it is ambiguous

Under that rule `a = 1 OR b = 2 AND c = 3` has two different parse trees, which mean different things:

```
       OR                          AND
     /    \                      /     \
  a = 1    AND                 OR       c = 3
          /   \               /  \
       b = 2   c = 3       a = 1  b = 2

   a = 1 OR (b = 2 AND c = 3)    (a = 1 OR b = 2) AND c = 3
```

A query must have one meaning, so the fix is to stratify: one nonterminal per precedence level, with lower-precedence operators nearer the root. An `and_expr` cannot contain a bare `OR`, since the only way to reach `OR` from below `expr` is through the parenthesized `"(" expr ")"`. So only the left tree above can be derived, and this is what makes `AND` bind more tightly than `OR` as a matter of grammar, with no precedence table in the parser.

### Problem 2: it is left-recursive

A rule such as `additive -> additive "+" multiplicative` sends a recursive-descent parser into infinite recursion, because it calls itself before consuming any input. Rewriting it as iteration, `additive -> multiplicative { "+" multiplicative }`, derives the same strings. The grammar no longer records associativity, so the translation step restores it, folding each repetition to the left. That is why `1 - 2 - 3` means `(1 - 2) - 3`, and a test checks this.

### Chained comparisons are rejected, not guessed

`a < b < c` looks reasonable, but a stratified rule that allowed it would make it mean `(a < b) < c`, which compares a boolean with a value: certainly not what anybody meant. `comparison` therefore allows at most one operator, so `a < b < c` has **no derivation at all**, and the error tells the user to write `a < b AND b < c`. Explicit grouping such as `(a < b) = TRUE` is still allowed, as it is unambiguous.

## Why this closes off SQL injection

The classic attack depends on a value being pasted into query text, where it gets reparsed as structure. Here that cannot happen as long as clients use parameters:

1. The text, with `?` placeholders, is parsed once into a tree, whose shape is then fixed.
2. Values travel separately, and are bound at evaluation time. The `Param` node holds a position, never text.
3. `' OR '1'='1` is therefore one string compared with a column, which matches nothing.

The grammar also derives exactly one statement per request, so `x; DROP TABLE t` is a syntax error whatever the input. `TestParametersCannotChangeQueryStructure` runs real payloads through the full executor.

The guarantee has a condition: an application that builds a query by gluing a user's value into the text has opted out of it, and no database can prevent that.

## Bounds on hostile input

The parser limits query size (1 MiB) and nesting depth (200), so `((((((...` is an error and not a stack overflow. `TestDeepNestingIsBounded` covers this.

## Limits of this language

There are no aggregates, subqueries, `OUTER JOIN`, `UNIQUE`, `DROP` or `ALTER`. A foreign key must reference a single-column primary key, and primary-key columns cannot be updated. These are deliberate limits of the project's scope.
