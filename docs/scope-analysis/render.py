#!/usr/bin/env python3
"""Render analyze.go default/integration JSON into reviewable inventories."""
import collections
import csv
import json
import pathlib
import sys

outdir = pathlib.Path(__file__).resolve().parent
runs = [json.load(open(p)) for p in sys.argv[1:]]
if not runs or any(r['Errors'] for r in runs):
    raise SystemExit('Both analysis runs must load without errors')
nodes = {}
for run in runs:
    for key, node in run['Nodes'].items():
        if key in nodes:
            node['Edges'] = sorted(set(node['Edges'] or []) | set(nodes[key]['Edges'] or []))
        nodes[key] = node
imports = runs[0]['Packages']

def closure(roots, allowed=None):
    seen, todo = set(), list(roots)
    while todo:
        key = todo.pop()
        if key in seen or key not in nodes:
            continue
        if allowed is not None and nodes[key]['Package'] not in allowed:
            continue
        seen.add(key)
        todo.extend(nodes[key]['Edges'] or [])
    return seen

def import_closure(pkg):
    seen, todo = set(), [pkg]
    while todo:
        p = todo.pop()
        if p not in seen:
            seen.add(p)
            todo.extend(imports.get(p) or [])
    return seen

def short(p):
    return p.removeprefix('github.com/richiejp/darkaifactory/')

def label(key):
    n = nodes[key]
    return f"{short(n['Package'])}.{n['Name']}@{n['File']}:{n['Line']}"

components = {}
for component, roots in runs[0]['Roots'].items():
    pkgs = import_closure('github.com/richiejp/darkaifactory/cmd/' + {'client':'daft','sandboxd':'daft-sandboxd','guest':'daft-agent'}[component])
    reach = closure(roots, pkgs)
    funcs = {i for i in reach if nodes[i]['Kind'] == 'function' and not nodes[i]['Test']}
    components[component] = (pkgs, reach, funcs)
reverse = collections.defaultdict(set)
for key, node in nodes.items():
    for target in node['Edges'] or []:
        reverse[target].add(key)

def write_tsv(name, header, rows):
    with (outdir/name).open('w') as f:
        w = csv.writer(f, delimiter='\t', lineterminator='\n')
        w.writerow(header)
        w.writerows(rows)

write_tsv('reference-edges.tsv', ['from', 'from_kind', 'from_test_file', 'to', 'to_kind'],
          ((label(k), n['Kind'], n['Test'], label(t), nodes[t]['Kind'])
           for k,n in sorted(nodes.items()) for t in n['Edges'] or []))
root_tests = [k for k,n in nodes.items() if n['Test'] and n['Kind']=='function' and n['Name'].startswith(('Test','Example','Benchmark','Fuzz'))]
test_rows = []
for test in sorted(root_tests):
    # Traverse test helpers, stopping at production boundaries. This identifies
    # exact production references made by tests without treating production
    # callbacks as coverage evidence.
    seen, todo, frontier = set(), [test], set()
    while todo:
        k = todo.pop()
        if k in seen: continue
        seen.add(k)
        for t in nodes[k]['Edges'] or []:
            if nodes[t]['Test']: todo.append(t)
            else: frontier.add(t)
    for comp, (pkgs,reach,funcs) in components.items():
        hit = frontier & reach
        if hit:
            n=nodes[test]
            test_rows.append((comp,n['File'],n['Line'],n['Name'],'; '.join(label(i) for i in sorted(hit)), 'integration' if test not in runs[0]['Nodes'] else 'default'))
write_tsv('tests.tsv', ['component','test_file','line','test_or_benchmark','production_references_through_test_helpers','build_selection'],test_rows)

for comp,(pkgs,reach,funcs) in components.items():
    groups=collections.defaultdict(list)
    for key in funcs: groups[nodes[key]['Package']].append(key)
    tests=[r for r in test_rows if r[0]==comp]
    lines=[f'# {comp.capitalize()} function and package inventory','',
           f'{len(funcs)} production function declarations in {len(groups)} packages are reached by the typed reference traversal. The production import closure includes {sum(p.startswith("github.com/richiejp/darkaifactory/") for p in pkgs)} repository packages. {len(tests)} test/benchmark/fuzz/example roots reference reached declarations, directly or through test helpers.','',
           'This is a conservative reference graph, including registered callbacks and interface implementations; it is not proof of execution or test coverage. Anonymous functions are folded into their enclosing declaration. Generated functions are included. See [method and limitations](../scope-reduction.md#method-and-limits).','',
           'Full recursive edges: [reference-edges.tsv](reference-edges.tsv). Named test roots and their production references: [tests.tsv](tests.tsv). The test-caller column below includes test helpers as well as test roots.','',
           '## Imported repository packages','',
           '\n'.join('- `'+short(p)+'`' for p in sorted(pkgs) if p.startswith('github.com/richiejp/darkaifactory/')),'',
           '## Functions by package','']
    for pkg,keys in sorted(groups.items()):
        lines += [f'### {short(pkg)}','', '| Function | Definition | Direct callers in test files |','| --- | --- | --- |']
        for key in sorted(keys, key=lambda k:(nodes[k]['File'],nodes[k]['Line'])):
            n=nodes[key]
            callers=sorted({nodes[t]['Name']+' ('+nodes[t]['File']+':'+str(nodes[t]['Line'])+')' for t in reverse[key] if nodes[t]['Test']})
            lines.append(f"| `{n['Name']}` | [{n['File']}:{n['Line']}](../../{n['File']}) | "+ ('; '.join('`'+c+'`' for c in callers) or '—')+' |')
        lines.append('')
    (outdir/(comp+'.md')).write_text('\n'.join(lines)+'\n')

rows=[]
for p in sorted(imports):
    if '/internal/' not in p: continue
    rows.append([short(p)] + [str(p in components[c][0]).lower() for c in ['sandboxd','client','guest']] + [sum(nodes[i]['Package']==p for i in components[c][2]) for c in ['sandboxd','client','guest']])
write_tsv('packages.tsv',['package','sandboxd_import','client_import','guest_import','sandboxd_functions','client_functions','guest_functions'],rows)
print({c:{'functions':len(v[2]),'function_packages':len({nodes[i]['Package'] for i in v[2]}),'imports':sum(p.startswith('github.com/richiejp/darkaifactory/') for p in v[0])} for c,v in components.items()})
print('test associations',len(test_rows),'graph nodes',len(nodes))
