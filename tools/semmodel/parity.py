# Writes the parity references internal/semantic tests compare against: the
# precompiled charsmap of the tokenizer, every phrase normalized as the
# reference tokenizer normalizes it, its token ids, and its model2vec vector.
#   python tools/semmodel/parity.py <snapshot dir> internal/semantic/testdata
import base64, io, json, os, random, struct, sys
from tokenizers import Tokenizer
from tokenizers.normalizers import Normalizer

snap, out = sys.argv[1], sys.argv[2]
tok_json = json.load(open(os.path.join(snap, 'tokenizer.json'), encoding='utf-8'))

def find_charsmap(n):
    if isinstance(n, dict):
        if n.get('type') == 'Precompiled':
            return n['precompiled_charsmap']
        for c in n.get('normalizers', []):
            got = find_charsmap(c)
            if got:
                return got
    return None

open(os.path.join(out, 'charsmap.bin'), 'wb').write(base64.b64decode(find_charsmap(tok_json['normalizer'])))

random.seed(7)
words = ['кофе', 'машина', 'дома', 'купить', 'чайник', 'электрический', 'coffee', 'maker', 'kettle',
         'best', 'review', 'Kaffeemaschine', 'kaufen', 'máquina', 'café', 'кавоварка', 'купити',
         'нейросеть', 'фото', 'видео', 'online', 'free', 'iphone', '15', 'pro', '2026']
extras = ['Coffee Maker', 'кофе, машина!', 'a/b', '"цитата"', 'кофе   машина', 'ｃｏｆｆｅｅ', 'ﬁlter',
          'и\u0306огурт', 'кофе ☕', 'iphone 15 pro', '', ' ', 'КОФЕ МАШИНА', 'café-bar', "it's", '100%',
          'tab\u2003separated', 'кофе\u00a0машина', 'x²', '①②', 'Ⅻ',
          'ｃ\u0301afe', 'ﬁ\u0301lter', 'ｅ\u0301\u0302\u0303 test']
phrases = list(extras)
while len(phrases) < 2000:
    n = random.randint(1, 4)
    phrases.append(' '.join(random.choice(words) for _ in range(n)))
with io.open(os.path.join(out, 'phrases.txt'), 'w', encoding='utf-8', newline='\n') as f:
    for p in phrases:
        f.write(p.replace('\n', ' ') + '\n')

tk = Tokenizer.from_file(os.path.join(snap, 'tokenizer.json'))
with io.open(os.path.join(out, 'normalized.tsv'), 'w', encoding='utf-8', newline='\n') as f:
    for p in phrases:
        norm = tk.normalizer.normalize_str(p)
        pre = ''.join(s for s, _ in tk.pre_tokenizer.pre_tokenize_str(norm))
        f.write(p + '\t' + pre + '\n')

unk = tk.token_to_id('[UNK]')
with io.open(os.path.join(out, 'tokens.tsv'), 'w', encoding='utf-8', newline='\n') as f:
    for p in phrases:
        ids = [i for i in tk.encode(p, add_special_tokens=False).ids if i != unk]
        f.write(p + '\t' + ' '.join(map(str, ids)) + '\n')

from model2vec import StaticModel
m = StaticModel.from_pretrained(snap)
vecs = m.encode(phrases[:300])
with open(os.path.join(out, 'vectors.bin'), 'wb') as f:
    f.write(struct.pack('<II', vecs.shape[0], vecs.shape[1]))
    f.write(vecs.astype('<f4').tobytes())
print('ok', len(phrases))
